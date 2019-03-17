package punch

import (
	"context"
	"time"

	ggio "github.com/gogo/protobuf/io"
	autonat "github.com/libp2p/go-libp2p-autonat"
	circuit "github.com/libp2p/go-libp2p-circuit"
	inet "github.com/libp2p/go-libp2p-net"
	peer "github.com/libp2p/go-libp2p-peer"
	swarm "github.com/libp2p/go-libp2p-swarm"
	basic "github.com/libp2p/go-libp2p/p2p/host/basic"
	pb "github.com/libp2p/go-libp2p/p2p/host/punch/pb"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr-net"
)

type Punch struct {
	*basic.BasicHost
}

const PunchProtocol = "/p2p/punch/1.0.0"
const PunchRetry = 5

func NewPunch(bhost *basic.BasicHost) *Punch {
	p := &Punch{
		bhost,
	}
	bhost.SetStreamHandler(PunchProtocol, p.PunchInfoHandler)
	bhost.Network().Notify(p)
	return p
}

func (p *Punch) GetPunchInfo(peerID peer.ID) ([]ma.Multiaddr, autonat.NATStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := p.NewStream(ctx, peerID, PunchProtocol)
	if err != nil {
		return nil, autonat.NATStatusUnknown, err
	}

	defer inet.FullClose(s)

	r := ggio.NewDelimitedReader(s, 2048)
	mes := pb.Punch{}
	if err := r.ReadMsg(&mes); err != nil {
		s.Reset()
		return nil, autonat.NATStatusUnknown, err
	}

	multiaddrs := make([]ma.Multiaddr, 0)

	for _, mas := range mes.Addresses {
		ma, err := ma.NewMultiaddr(mas)
		if err != nil {
			continue
		}

		multiaddrs = append(multiaddrs, ma)
	}

	return multiaddrs, autonat.NATStatus(*mes.NatStatus), nil
}

func (p *Punch) PunchInfoHandler(s inet.Stream) {
	defer inet.FullClose(s)
	natstatus := int32(p.AutoNat().Status())

	w := ggio.NewDelimitedWriter(s)
	mes := pb.Punch{
		Addresses: make([]string, 0),
		NatStatus: &natstatus,
	}
	for _, addr := range p.AllAddrs() {
		if manet.IsPrivateAddr(addr) {
			continue
		}

		if _, err := addr.ValueForProtocol(circuit.P_CIRCUIT); err == nil {
			continue
		}
		if _, err := addr.ValueForProtocol(ma.P_QUIC); err != nil {
			continue
		}

		mes.Addresses = append(mes.Addresses, addr.String())
	}
	w.WriteMsg(&mes)
}

func (p *Punch) Listen(inet.Network, ma.Multiaddr)      {}
func (p *Punch) ListenClose(inet.Network, ma.Multiaddr) {}
func (p *Punch) Connected(_ inet.Network, c inet.Conn) {
	// Break if we are public, because we don't need any action
	// with hole punching, just let peer connect us.
	if p.AutoNat().Status() == autonat.NATStatusPublic {
		return
	}
	// Forget the direct connection.
	if _, err := c.RemoteMultiaddr().ValueForProtocol(circuit.P_CIRCUIT); err != nil {
		return
	}
	peerID := c.RemotePeer()
	go func() {
		addrs, _, err := p.GetPunchInfo(peerID)
		if err != nil || len(addrs) == 0 {
			return
		}
		punch := func() {
			var s *swarm.Swarm
			var ok bool
			s, ok = p.Network().(*swarm.Swarm)
			if !ok {
				return
			}
			s.Backoff().Clear(peerID)

			//peerInfo := pstore.PeerInfo{
			//	ID:    peerID,
			//	Addrs: addrs,
			//}
			for i := 0; i < PunchRetry; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_, err := s.DialDirect(ctx, peerID, addrs)
				if err != nil {
					continue
				}
				// TODO: stream migrate
				c.Close()
				return
			}
		}
		punch()
	}()
}

func (p *Punch) Disconnected(_ inet.Network, c inet.Conn) {}

func (p *Punch) OpenedStream(inet.Network, inet.Stream) {}
func (p *Punch) ClosedStream(inet.Network, inet.Stream) {}
