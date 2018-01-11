package x

import (
	"errors"
	"fmt"
	"net"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/republicprotocol/go-dht"
	"github.com/republicprotocol/go-identity"
	"github.com/republicprotocol/go-x/rpc"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

// α determines the maximum number of concurrent client connections that the
// Node is expected to use when running a distributed Dijkstra search.
const α = 3

// Node implements the gRPC Node service.
type Node struct {
	*grpc.Server
	Delegate
	MultiAddress identity.MultiAddress
	DHT          *dht.DHT
}

// The Delegate is used to inject dependencies into the RPC logic.
type Delegate interface {
	OnPingReceived(peer identity.MultiAddress)
	OnOrderFragmentReceived()
}

// NewNode returns a Node with the given Config, a new DHT, and a new set of grpc.Connections.
func NewNode(multi identity.MultiAddress, multis identity.MultiAddresses, delegate Delegate) (*Node, error) {
	address, err := multi.Address()
	if err != nil {
		return nil, err
	}
	dht := dht.NewDHT(address)
	for _, multi := range multis {
		if err := dht.Update(multi); err != nil {
			return nil, err
		}
	}
	return &Node{
		Server:       grpc.NewServer(),
		Delegate:     delegate,
		MultiAddress: multi,
		DHT:          dht,
	}, nil
}

// Serve starts the gRPC server.
func (node *Node) Serve() error {
	host, err := node.MultiAddress.ValueForProtocol(identity.IP4Code)
	if err != nil {
		return err
	}
	port, err := node.MultiAddress.ValueForProtocol(identity.TCPCode)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("%s:%s", host, port))
	if err != nil {
		return err
	}
	rpc.RegisterNodeServer(node.Server, node)
	reflection.Register(node.Server)
	return node.Server.Serve(listener)
}

// Ping is used to test the connection to the Node and exchange MultiAddresses.
// If the Node does not respond, or it responds with an error, then the
// connection is considered unhealthy.
func (node *Node) Ping(ctx context.Context, peer *rpc.MultiAddress) (*rpc.MultiAddress, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Spawn a goroutine to evaluate the return value.
	wait := make(chan error)
	go func() {
		defer close(wait)
		wait <- node.handlePing(peer)
	}()

	select {
	case ret := <-wait:
		return &rpc.MultiAddress{Multi: node.MultiAddress.String()}, ret

	// Select the timeout from the context.
	case <-ctx.Done():
		return &rpc.MultiAddress{Multi: node.MultiAddress.String()}, ctx.Err()
	}
}

// Peers is used to return the rpc.MultiAddresses to which a Node is connected.
// The rpc.MultiAddresses returned are not guaranteed to provide healthy
// connections and should be pinged.
func (node *Node) Peers(ctx context.Context, sender *rpc.Nothing) (*rpc.MultiAddresses, error) {
	if err := ctx.Err(); err != nil {
		return &rpc.MultiAddresses{}, err
	}

	// Spawn a goroutine to evaluate the return value.
	wait := make(chan *rpc.MultiAddresses)
	go func() {
		defer close(wait)
		wait <- node.handlePeers()
	}()

	select {
	case ret := <-wait:
		return ret, nil

	// Select the timeout from the context.
	case <-ctx.Done():
		return &rpc.MultiAddresses{}, ctx.Err()
	}
}

// SendOrderFragment is used to forward an rpc.OrderFragment through the X
// Network to its destination Node. This forwarding is done using a distributed
// Dijkstra search, using the XOR distance between identity.Addresses as the
// distance heuristic.
func (node *Node) SendOrderFragment(ctx context.Context, orderFragment *rpc.OrderFragment) (*rpc.MultiAddress, error) {
	// Check for errors in the context.
	if err := ctx.Err(); err != nil {
		return &rpc.MultiAddress{Multi: node.MultiAddress.String()}, err
	}

	// Spawn a goroutine to evaluate the return value.
	type retType struct {
		*rpc.MultiAddress
		error
	}
	wait := make(chan retType)
	go func() {
		defer close(wait)
		multi, err := node.handleSendOrderFragment(orderFragment)
		wait <- retType{multi, err}
	}()

	select {
	// Select the timeout from the context.
	case <-ctx.Done():
		return &rpc.MultiAddress{Multi: node.MultiAddress.String()}, ctx.Err()

	// Select the value passed by the goroutine.
	case ret := <-wait:
		return ret.MultiAddress, ret.error
	}
}

// ForwardOrderFragment forwards the order fragment to the miners so that they
// can transmit the order fragment to the target. Return nil if forward
// successfully, or an error.
func (node *Node) ForwardOrderFragment(orderFragment *rpc.OrderFragment) error {

	target := identity.Address(orderFragment.To)
	open := node.DHT.MultiAddresses()
	if len(open) == 0 {
		return errors.New("empty dht")
	}

	// Sort the nodes we already know
	sort.SliceStable(open, func(i, j int) bool {
		left, _ := open[i].Address()
		right, _ := open[j].Address()
		closer, _ := identity.Closer(left, right, target)
		return closer
	})

	// If we know the target,send the order fragment to the target directly
	closestNode, err := open[0].Address()
	if err != nil {
		return err
	}
	if string(closestNode) == string(target) {
		_, err := node.RPCSendOrderFragment(open[0], orderFragment)
		return err
	}

	// Otherwise forward the fragment to the closest α nodes simultaneously
	for len(open) > 0 {
		asyncRoutines := len(open)
		if len(open) > 3 {
			asyncRoutines = α
		}

		var wg sync.WaitGroup
		wg.Add(asyncRoutines)
		targetFound := int32(0)

		// Forward order fragment
		for i := 0; i < asyncRoutines; i++ {
			multi := open[0]
			open = open[1:]
			go func() {
				defer wg.Done()
				response, _ := node.RPCSendOrderFragment(multi, orderFragment)
				if response != nil {
					atomic.StoreInt32(&targetFound, 1)
				}
			}()
			if len(open) == 0 {
				break
			}
		}
		wg.Wait()
		if targetFound != 0 {
			return nil
		}
	}
	return errors.New("we can't find the target")
}

func (node *Node) handlePing(peer *rpc.MultiAddress) error {
	multi, err := identity.NewMultiAddressFromString(peer.Multi)
	if err != nil {
		return err
	}
	node.Delegate.OnPingReceived(multi)

	// Attempt to update the DHT.
	err = node.DHT.Update(multi)
	if err == dht.ErrFullBucket {
		// If the DHT is full then try and prune disconnected peers.
		address, err := multi.Address()
		if err != nil {
			return err
		}
		pruned, err := node.pruneMostRecentPeer(address)
		if err != nil {
			return err
		}
		// If a peer was pruned, then update the DHT again.
		if pruned {
			return node.DHT.Update(multi)
		}
		return nil
	}
	return err
}

func (node *Node) handlePeers() *rpc.MultiAddresses {
	// Get all identity.MultiAddresses in the DHT.
	multis := node.DHT.MultiAddresses()
	ret := &rpc.MultiAddresses{
		Multis: make([]*rpc.MultiAddress, len(multis)),
	}
	// Transform them into rpc.MultiAddresses.
	for i, multi := range multis {
		ret.Multis[i] = &rpc.MultiAddress{Multi: multi.String()}
	}
	return ret
}

func (node *Node) handleSendOrderFragment(orderFragment *rpc.OrderFragment) (*rpc.MultiAddress, error) {

	target := identity.Address(orderFragment.To)
	if target == node.DHT.Address {
		node.Delegate.OnOrderFragmentReceived()
		return &rpc.MultiAddress{Multi: node.MultiAddress.String()}, nil
	}

	targetMultiMu := new(sync.Mutex)
	var targetMulti *identity.MultiAddress

	// Initialize the closed list
	closed := make(map[string]bool)
	self, err := node.MultiAddress.Address()
	if err != nil {
		return nil, err

	}
	closed[self] = true
	multiFrom, err := identity.NewMultiAddressFromString(orderFragment.From)
	if err != nil {
		return nil, err
	}
	from, err := multiFrom.Address()
	if err != nil {
		return nil, err
	}
	closed[from] = true

	// Initialize the open list
	openMu := new(sync.Mutex)
	open := identity.MultiAddresses{}
	for _, peer := range node.DHT.MultiAddresses() {
		address, err := peer.Address()
		if err != nil {
			return nil, err
		}
		if _, ok := closed[address]; !ok {
			open = append(open, peer)
		}
	}
	if len(open) == 0 {
		return nil, errors.New("empty dht")
	}
	sort.Slice(open, func(i, j int) bool {
		left, _ := open[i].Address()
		right, _ := open[j].Address()
		closer, _ := identity.Closer(left, right, target)
		return closer
	})

	// If we know the target,send the order fragment to the target directly
	closestNode, err := open[0].Address()
	if err != nil {
		return nil, err
	}
	if closestNode == target {
		_, err := node.RPCSendOrderFragment(open[0], orderFragment)
		return &rpc.MultiAddress{Multi: open[0].String()}, err
	}

	// TODO: We are only using one dht.Bucket to search the network. If this
	//       dht.Bucket is not sufficient, we should also search the
	//       neighborhood of the dht.Bucket. The neighborhood should expand by
	//       a distance of one, until the entire DHT has been searched, or the
	//       targetMulti has been found.
	for len(open) > 0 {
		asyncRoutines := len(open)
		if len(open) > 3 {
			asyncRoutines = α
		}
		var wg sync.WaitGroup
		wg.Add(asyncRoutines)

		// Take the first α multi-addresses from the open list and expand them
		// concurrently. This moves them from the open list to the closed list,
		// preventing the same multi-address from being expanded more than
		// once.
		for i := 0; i < asyncRoutines; i++ {
			if len(open) == 0 {
				break
			}
			multi := open[0]
			open = open[1:]
			address, err := multi.Address()
			if err != nil {
				return nil, err
			}
			closed[address] = true

			go func() {
				defer wg.Done()

				// Get all peers of this multi-address. This is the expansion
				// step of the search.
				peers, err := node.RPCPeers(multi)
				if err != nil {
					return
				}

				// Traverse all peers and collect them into the openNext, a
				// list of peers that we want to add the open list.
				openNext := make([]identity.MultiAddress, 0, len(peers))
				for _, peer := range peers {
					address, err := peer.Address()
					if err != nil {
						return
					}
					if target == address {
						// If we have found the target, set the targetMulti and
						// exit the loop. There is no point acquiring more
						// peers for the open list.
						targetMultiMu.Lock()
						if targetMulti == nil {
							targetMulti = &peer
						}
						targetMultiMu.Unlock()
						break
					}
					// Otherwise, store this peer's multi-address in the
					// openNext list. It will be added to the open list if it
					// has not already been closed.
					openNext = append(openNext, peer)
				}

				targetMultiMu.Lock()
				if targetMulti == nil {
					// We have not found the targetMulti yet.
					targetMultiMu.Unlock()
					openMu.Lock()
					// Add new peers to the open list if they have not been
					// closed.
					for _, next := range openNext {
						address, err := next.Address()
						if err != nil {
							return
						}
						if _, ok := closed[address]; !ok {
							open = append(open, next)
						}
					}
					openMu.Unlock()
				} else {
					targetMultiMu.Unlock()
				}
			}()
		}
		wg.Wait()
		if targetMulti != nil {
			// If we have found the targetMulti then we can exit the search
			// loop.
			break
		}

		// Otherwise, sort the open list by distance to the target.
		sort.Slice(open, func(i, j int) bool {
			left, _ := open[i].Address()
			right, _ := open[j].Address()
			closer, _ := identity.Closer(left, right, target)
			return closer
		})
	}

	if targetMulti == nil {
		return nil, fmt.Errorf("cannot find target")
	}
	response, err := node.RPCSendOrderFragment(*targetMulti, orderFragment)
	if err != nil {
		return nil, err
	}

	return &rpc.MultiAddress{Multi: response.String()}, nil
}

func (node *Node) pruneMostRecentPeer(target identity.Address) (bool, error) {
	bucket, err := node.DHT.FindBucket(target)
	if err != nil {
		return false, err
	}
	multi := bucket.OldestMultiAddress()
	if multi == nil {
		return false, nil
	}
	newMulti, err := node.RPCPing(*multi)
	if err != nil {
		return true, node.DHT.Remove(*multi)
	}
	node.DHT.Update(*newMulti)
	return false, nil
}
