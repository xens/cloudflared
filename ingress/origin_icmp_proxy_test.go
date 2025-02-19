package ingress

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"strings"
	"testing"

	"github.com/google/gopacket/layers"
	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"
	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"

	"github.com/cloudflare/cloudflared/packet"
)

var (
	noopLogger    = zerolog.Nop()
	localhostIP   = netip.MustParseAddr("127.0.0.1")
	localhostIPv6 = netip.MustParseAddr("::1")
)

// TestICMPProxyEcho makes sure we can send ICMP echo via the Request method and receives response via the
// ListenResponse method
//
// Note: if this test fails on your device under Linux, then most likely you need to make sure that your user
// is allowed in ping_group_range. See the following gist for how to do that:
// https://github.com/ValentinBELYN/icmplib/blob/main/docs/6-use-icmplib-without-privileges.md
func TestICMPProxyEcho(t *testing.T) {
	testICMPProxyEcho(t, true)
	testICMPProxyEcho(t, false)
}

func testICMPProxyEcho(t *testing.T, sendIPv4 bool) {
	const (
		echoID = 36571
		endSeq = 20
	)

	proxy, err := NewICMPProxy(&noopLogger)
	require.NoError(t, err)

	proxyDone := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		proxy.Serve(ctx)
		close(proxyDone)
	}()

	responder := echoFlowResponder{
		decoder:  packet.NewICMPDecoder(),
		respChan: make(chan []byte, 1),
	}

	protocol := layers.IPProtocolICMPv6
	if sendIPv4 {
		protocol = layers.IPProtocolICMPv4
	}
	localIPs := getLocalIPs(t, sendIPv4)
	ips := make([]*packet.IP, len(localIPs))
	for i, localIP := range localIPs {
		ips[i] = &packet.IP{
			Src:      localIP,
			Dst:      localIP,
			Protocol: protocol,
		}
	}

	var icmpType icmp.Type = ipv6.ICMPTypeEchoRequest
	if sendIPv4 {
		icmpType = ipv4.ICMPTypeEcho
	}
	for seq := 0; seq < endSeq; seq++ {
		for i, ip := range ips {
			pk := packet.ICMP{
				IP: ip,
				Message: &icmp.Message{
					Type: icmpType,
					Code: 0,
					Body: &icmp.Echo{
						ID:   echoID + i,
						Seq:  seq,
						Data: []byte(fmt.Sprintf("icmp echo seq %d", seq)),
					},
				},
			}
			require.NoError(t, proxy.Request(&pk, &responder))
			responder.validate(t, &pk)
		}
	}
	cancel()
	<-proxyDone
}

// TestICMPProxyRejectNotEcho makes sure it rejects messages other than echo
func TestICMPProxyRejectNotEcho(t *testing.T) {
	msgs := []icmp.Message{
		{
			Type: ipv4.ICMPTypeDestinationUnreachable,
			Code: 1,
			Body: &icmp.DstUnreach{
				Data: []byte("original packet"),
			},
		},
		{
			Type: ipv4.ICMPTypeTimeExceeded,
			Code: 1,
			Body: &icmp.TimeExceeded{
				Data: []byte("original packet"),
			},
		},
		{
			Type: ipv4.ICMPType(2),
			Code: 0,
			Body: &icmp.PacketTooBig{
				MTU:  1280,
				Data: []byte("original packet"),
			},
		},
	}
	testICMPProxyRejectNotEcho(t, localhostIP, msgs)
	msgsV6 := []icmp.Message{
		{
			Type: ipv6.ICMPTypeDestinationUnreachable,
			Code: 3,
			Body: &icmp.DstUnreach{
				Data: []byte("original packet"),
			},
		},
		{
			Type: ipv6.ICMPTypeTimeExceeded,
			Code: 0,
			Body: &icmp.TimeExceeded{
				Data: []byte("original packet"),
			},
		},
		{
			Type: ipv6.ICMPTypePacketTooBig,
			Code: 0,
			Body: &icmp.PacketTooBig{
				MTU:  1280,
				Data: []byte("original packet"),
			},
		},
	}
	testICMPProxyRejectNotEcho(t, localhostIPv6, msgsV6)
}

func testICMPProxyRejectNotEcho(t *testing.T, srcDstIP netip.Addr, msgs []icmp.Message) {
	proxy, err := NewICMPProxy(&noopLogger)
	require.NoError(t, err)

	responder := echoFlowResponder{
		decoder:  packet.NewICMPDecoder(),
		respChan: make(chan []byte),
	}
	protocol := layers.IPProtocolICMPv4
	if srcDstIP.Is6() {
		protocol = layers.IPProtocolICMPv6
	}
	for _, m := range msgs {
		pk := packet.ICMP{
			IP: &packet.IP{
				Src:      srcDstIP,
				Dst:      srcDstIP,
				Protocol: protocol,
			},
			Message: &m,
		}
		require.Error(t, proxy.Request(&pk, &responder))
	}
}

type echoFlowResponder struct {
	decoder  *packet.ICMPDecoder
	respChan chan []byte
}

func (efr *echoFlowResponder) SendPacket(dst netip.Addr, pk packet.RawPacket) error {
	copiedPacket := make([]byte, len(pk.Data))
	copy(copiedPacket, pk.Data)
	efr.respChan <- copiedPacket
	return nil
}

func (efr *echoFlowResponder) Close() error {
	close(efr.respChan)
	return nil
}

func (efr *echoFlowResponder) validate(t *testing.T, echoReq *packet.ICMP) {
	pk := <-efr.respChan
	decoded, err := efr.decoder.Decode(packet.RawPacket{Data: pk})
	require.NoError(t, err)
	require.Equal(t, decoded.Src, echoReq.Dst)
	require.Equal(t, decoded.Dst, echoReq.Src)
	require.Equal(t, echoReq.Protocol, decoded.Protocol)

	if echoReq.Type == ipv4.ICMPTypeEcho {
		require.Equal(t, ipv4.ICMPTypeEchoReply, decoded.Type)
	} else {
		require.Equal(t, ipv6.ICMPTypeEchoReply, decoded.Type)
	}
	require.Equal(t, 0, decoded.Code)
	if echoReq.Type == ipv4.ICMPTypeEcho {
		require.NotZero(t, decoded.Checksum)
	} else {
		// For ICMPv6, the kernel will compute the checksum during transmission unless pseudo header is not nil
		require.Zero(t, decoded.Checksum)
	}

	require.Equal(t, echoReq.Body, decoded.Body)
}

func getLocalIPs(t *testing.T, ipv4 bool) []netip.Addr {
	interfaces, err := net.Interfaces()
	require.NoError(t, err)
	localIPs := []netip.Addr{}
	for _, i := range interfaces {
		// Skip TUN devices
		if strings.Contains(i.Name, "tun") {
			continue
		}
		addrs, err := i.Addrs()
		require.NoError(t, err)
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && (ipnet.IP.IsPrivate() || ipnet.IP.IsLoopback()) {
				if (ipv4 && ipnet.IP.To4() != nil) || (!ipv4 && ipnet.IP.To4() == nil) {
					localIPs = append(localIPs, netip.MustParseAddr(ipnet.IP.String()))
				}
			}
		}
	}
	return localIPs
}
