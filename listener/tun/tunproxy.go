package tun

import (
	"fmt"
	"net"
	"net/url"
	"strconv"

	"github.com/Dreamacro/clash/adapter/inbound"
	C "github.com/Dreamacro/clash/constant"
	"github.com/Dreamacro/clash/listener/tun/dev"
	"github.com/Dreamacro/clash/log"
	"github.com/Dreamacro/clash/transport/socks5"

	"encoding/binary"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv6"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

const nicID tcpip.NICID = 1

// tunAdapter is the wraper of tun
type tunAdapter struct {
	device  dev.TunDevice
	ipstack *stack.Stack

	udpInbound chan<- *inbound.PacketAdapter

	dnsserver *DNSServer
}

// NewTunProxy create TunProxy under Linux OS.
func NewTunProxy(deviceURL string, tcpIn chan<- C.ConnContext, udpIn chan<- *inbound.PacketAdapter) (TunAdapter, error) {

	var err error

	url, err := url.Parse(deviceURL)
	if err != nil {
		return nil, fmt.Errorf("invalid tun device url: %v", err)
	}

	tundev, err := dev.OpenTunDevice(*url)
	if err != nil {
		return nil, fmt.Errorf("can't open tun: %v", err)
	}

	ipstack := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, ipv6.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})

	tl := &tunAdapter{
		device:     tundev,
		ipstack:    ipstack,
		udpInbound: udpIn,
	}

	linkEP, err := tundev.AsLinkEndpoint()
	if err != nil {
		return nil, fmt.Errorf("unable to create virtual endpoint: %v", err)
	}

	if err := ipstack.CreateNIC(nicID, linkEP); err != nil {
		return nil, fmt.Errorf("fail to create NIC in ipstack: %v", err)
	}

	ipstack.SetPromiscuousMode(nicID, true) // Accept all the traffice from this NIC
	ipstack.SetSpoofing(nicID, true)        // Otherwise our TCP connection can not find the route backward

	// Add route for ipv4 & ipv6
	// So FindRoute will return correct route to tun NIC
	ipstack.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, Gateway: tcpip.Address{}, NIC: nicID})
	ipstack.AddRoute(tcpip.Route{Destination: header.IPv6EmptySubnet, Gateway: tcpip.Address{}, NIC: nicID})

	// TCP handler
	// maximum number of half-open tcp connection set to 1024
	// receive buffer size set to 20k
	tcpFwd := tcp.NewForwarder(ipstack, 20*1024, 1024, func(r *tcp.ForwarderRequest) {
		src := net.JoinHostPort(r.ID().RemoteAddress.String(), strconv.Itoa((int)(r.ID().RemotePort)))
		dst := net.JoinHostPort(r.ID().LocalAddress.String(), strconv.Itoa((int)(r.ID().LocalPort)))
		log.Debugln("Get TCP Syn %v -> %s in ipstack", src, dst)
		var wq waiter.Queue
		ep, err := r.CreateEndpoint(&wq)
		if err != nil {
			log.Warnln("Can't create TCP Endpoint(%s -> %s) in ipstack: %v", src, dst, err)
			r.Complete(true)
			return
		}
		r.Complete(false)

		conn := gonet.NewTCPConn(&wq, ep)

		// if the endpoint is not in connected state, conn.RemoteAddr() will return nil
		// this protection may be not enough, but will help us debug the panic
		if conn.RemoteAddr() == nil {
			log.Warnln("TCP endpoint is not connected, current state: %v", tcp.EndpointState(ep.State()))
			conn.Close()
			return
		}

		target := getAddr(ep.Info().(*stack.TransportEndpointInfo).ID)
		tcpIn <- inbound.NewSocket(target, conn, C.TUN)

	})
	ipstack.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpFwd.HandlePacket)

	// UDP handler
	ipstack.SetTransportProtocolHandler(udp.ProtocolNumber, tl.udpHandlePacket)

	log.Infoln("Tun adapter have interface name: %s", tundev.Name())
	return tl, nil

}

// Close close the TunAdapter
func (t *tunAdapter) Close() {
	t.device.Close()
	if t.dnsserver != nil {
		t.dnsserver.Stop()
	}
	t.ipstack.Close()
}

// IfName return device URL of tun
func (t *tunAdapter) DeviceURL() string {
	return t.device.URL()
}

func (t *tunAdapter) udpHandlePacket(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
	// ref: gvisor pkg/tcpip/transport/udp/endpoint.go HandlePacket
	hdr := header.UDP(pkt.TransportHeader().Slice())
	if int(hdr.Length()) > pkt.Data().Size()+header.UDPMinimumSize {
		// Malformed packet.
		t.ipstack.Stats().UDP.MalformedPacketsReceived.Increment()
		return true
	}

	target := getAddr(id)

	packet := &fakeConn{
		id:      id,
		pkt:     pkt,
		s:       t.ipstack,
		payload: pkt.Data().AsRange().ToSlice(),
	}
	t.udpInbound <- inbound.NewPacket(target, target.UDPAddr(), packet, C.TUN)

	return true
}

func getAddr(id stack.TransportEndpointID) socks5.Addr {
	local_addr := id.LocalAddress

	// get the big-endian binary represent of port
	port := make([]byte, 2)
	binary.BigEndian.PutUint16(port, id.LocalPort)

	if local_addr.Len() == 4 {
		addr := make([]byte, 1+net.IPv4len+2)
		addr[0] = socks5.AtypIPv4
		copy(addr[1:1+net.IPv4len], local_addr.AsSlice())
		addr[1+net.IPv4len], addr[1+net.IPv4len+1] = port[0], port[1]
		return addr
	} else {
		addr := make([]byte, 1+net.IPv6len+2)
		addr[0] = socks5.AtypIPv6
		copy(addr[1:1+net.IPv6len], local_addr.AsSlice())
		addr[1+net.IPv6len], addr[1+net.IPv6len+1] = port[0], port[1]
		return addr
	}

}
