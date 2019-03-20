package punch

import (
	"context"
	"errors"
	"time"

	ggio "github.com/gogo/protobuf/io"
	autonat "github.com/libp2p/go-libp2p-autonat"
	circuit "github.com/libp2p/go-libp2p-circuit"
	inet "github.com/libp2p/go-libp2p-net"
	peer "github.com/libp2p/go-libp2p-peer"
	pstore "github.com/libp2p/go-libp2p-peerstore"
	swarm "github.com/libp2p/go-libp2p-swarm"
	basic "github.com/libp2p/go-libp2p/p2p/host/basic"
	pb "github.com/libp2p/go-libp2p/p2p/host/punch/pb"
	ma "github.com/multiformats/go-multiaddr"
	manet "github.com/multiformats/go-multiaddr-net"
)

type Punch struct {
	*basic.BasicHost
	swarm *swarm.Swarm
}

const (
	PunchProtocol = "/p2p/punch/1.0.0"
	PunchRetry    = 5
	PunchInterval = 3
)

func NewPunch(bhost *basic.BasicHost) (*Punch, error) {
	s, ok := bhost.Network().(*swarm.Swarm)
	if !ok {
		return nil, errors.New("only support swarm")
	}
	p := &Punch{
		BasicHost: bhost,
		swarm:     s,
	}
	bhost.SetStreamHandler(PunchProtocol, p.PunchInfoHandler)
	bhost.Network().Notify(p)
	s.SetBestConn(p)
	s.SetBestDest(p)
	return p, nil
}

// GetPunchInfo Get peer's public addresses for quic
func (p *Punch) GetPunchInfo(peerID peer.ID) ([]ma.Multiaddr, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := p.NewStream(ctx, peerID, PunchProtocol)
	if err != nil {
		return nil, err
	}

	defer inet.FullClose(s)

	r := ggio.NewDelimitedReader(s, 2048)
	mes := pb.Punch{}
	if err := r.ReadMsg(&mes); err != nil {
		s.Reset()
		return nil, err
	}

	multiaddrs := make([]ma.Multiaddr, 0)

	for _, mas := range mes.Addresses {
		ma, err := ma.NewMultiaddr(mas)
		if err != nil {
			continue
		}

		multiaddrs = append(multiaddrs, ma)
	}

	return multiaddrs, nil
}

// PunchInfoHandler return the public address for quic
func (p *Punch) PunchInfoHandler(s inet.Stream) {
	defer inet.FullClose(s)

	w := ggio.NewDelimitedWriter(s)
	mes := pb.Punch{
		Addresses: make([]string, 0),
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

// findPublicAddrs Filter public address in a group of addresses
func (p *Punch) findPublicAddrs(addrs []ma.Multiaddr) []ma.Multiaddr {
	publicAddrs := make([]ma.Multiaddr, 0)
	for _, addr := range addrs {
		if _, err := addr.ValueForProtocol(circuit.P_CIRCUIT); err == nil {
			continue
		}

		if manet.IsPublicAddr(addr) {
			publicAddrs = append(publicAddrs, addr)
		}
	}
	return publicAddrs
}

// findPublicConns Filter public connection in a group of connections
func (p *Punch) findPublicConns(conns []*swarm.Conn) []*swarm.Conn {
	publicConns := make([]*swarm.Conn, 0)
	for _, conn := range conns {
		addr := conn.RemoteMultiaddr()
		if _, err := addr.ValueForProtocol(circuit.P_CIRCUIT); err == nil {
			continue
		}

		if conn.IsClosed() {
			continue
		}
		if manet.IsPublicAddr(addr) {
			publicConns = append(publicConns, conn)
		}
	}
	return publicConns
}

// ConnsToPeerSelect is same as the bestConnToPeer in go-libp2p-swarm
func (p *Punch) ConnsToPeerSelect(conns []*swarm.Conn) *swarm.Conn {
	var best *swarm.Conn
	bestLen := 0
	for _, conn := range conns {
		if conn.IsClosed() {
			continue
		}

		streams := conn.GetStreams()
		cLen := len(streams)
		if cLen >= bestLen {
			best = conn
			bestLen = cLen
		}
	}
	return best
}

// BestConn select the best conn in a group of connections
func (p *Punch) BestConn(id peer.ID, conns []*swarm.Conn) *swarm.Conn {
	addrs := p.swarm.Peerstore().Addrs(id)
	// 1. We find out that was a public address for peer, if not we just
	//    use ConnsToPeerSelect to select the connections.
	publicAddrs := p.findPublicAddrs(addrs)
	if len(publicAddrs) == 0 {
		return p.ConnsToPeerSelect(conns)
	}
	// 2. If Peer has the public address can dial and there is no public
	//    connections, we return nil. means that there isn't any best connection.
	publicConns := p.findPublicConns(conns)
	if len(publicConns) == 0 {
		return nil
	}

	// 3. If peer already has a public connection, return the public connection.
	return p.ConnsToPeerSelect(publicConns)
}

// BestConnFallback Use to find the connection if some error occur.
func (p *Punch) BestConnFallback(_ peer.ID, conns []*swarm.Conn) *swarm.Conn {
	return p.ConnsToPeerSelect(conns)

}

// BestDestSelect Preferred public address
func (p *Punch) BestDestSelect(id peer.ID, addrs []ma.Multiaddr) []ma.Multiaddr {
	// 1. If there isn't a connection yet. return all address, Let whatever a
	//    connection can create.
	peerConns := p.swarm.ConnsToPeer(id)
	if len(peerConns) == 0 {
		return addrs
	}
	// 2. If there isn't a public address, return all.
	publicAddrs := p.findPublicAddrs(addrs)
	if len(publicAddrs) == 0 {
		return addrs
	}
	return publicAddrs
}

func (p *Punch) Listen(inet.Network, ma.Multiaddr)      {}
func (p *Punch) ListenClose(inet.Network, ma.Multiaddr) {}
func (p *Punch) Connected(_ inet.Network, c inet.Conn) {
	// If we are a public NAT. We need do nothing, except wait connection.
	if p.AutoNat().Status() == autonat.NATStatusPublic {
		return
	}

	// Only CIRCUIT can indicate that we need to do hole punching.
	if _, err := c.RemoteMultiaddr().ValueForProtocol(circuit.P_CIRCUIT); err != nil {
		return
	}
	peerID := c.RemotePeer()
	go func() {
		addrs, err := p.GetPunchInfo(peerID)
		if err != nil {
			return
		}

		if len(addrs) != 0 {
			p.Peerstore().AddAddrs(peerID, addrs, pstore.TempAddrTTL)
		} else {
			return
		}
		punch := func() {
			p.swarm.Backoff().Clear(peerID)

			for i := 0; i < PunchRetry; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				punchconn, err := p.swarm.DialPeer(ctx, peerID)
				if err != nil {
					time.Sleep(PunchInterval)
					continue
				}

				// If we receive a CIRCUIT connection, it means that an error has occurred and try again.
				if _, err := punchconn.RemoteMultiaddr().ValueForProtocol(circuit.P_CIRCUIT); err == nil {
					time.Sleep(PunchInterval)
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
