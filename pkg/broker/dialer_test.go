package broker

import (
	"context"

	gc "github.com/go-check/check"
	"google.golang.org/grpc"

	pb "github.com/LiveRamp/gazette/pkg/protocol"
)

type DialerSuite struct{}

func (s *DialerSuite) TestDialer(c *gc.C) {
	var ctx, cancel = context.WithCancel(context.Background())
	defer cancel()

	var peer1, peer2 = newMockPeer(c, ctx), newMockPeer(c, ctx)

	var route = pb.Route{
		Brokers:   []pb.BrokerSpec_ID{{"zone-a", "member-1"}, {"zone-b", "member-2"}},
		Endpoints: []pb.Endpoint{pb.Endpoint("http://" + peer1.addr() + "/path"), pb.Endpoint("http://" + peer2.addr() + "/path")},
	}

	var d, err = newDialer(8)
	c.Check(err, gc.IsNil)

	var conn1a, conn1b, conn2 *grpc.ClientConn

	// Dial first peer.
	conn1a, err = d.dial(ctx, pb.BrokerSpec_ID{Zone: "zone-a", Suffix: "member-1"}, route)
	c.Check(conn1a, gc.NotNil)
	c.Check(err, gc.IsNil)

	// Dial second peer. Connection instances differ.
	conn2, err = d.dial(ctx, pb.BrokerSpec_ID{Zone: "zone-b", Suffix: "member-2"}, route)
	c.Check(conn2, gc.NotNil)
	c.Check(err, gc.IsNil)
	c.Check(conn1a, gc.Not(gc.Equals), conn2)

	// Dial first peer again. Expect the connection is cached and returned again.
	conn1b, err = d.dial(ctx, pb.BrokerSpec_ID{Zone: "zone-a", Suffix: "member-1"}, route)
	c.Check(err, gc.IsNil)
	c.Check(conn1a, gc.Equals, conn1b)

	// Expect an error for an ID not in the Route (even if it's cached).
	route = pb.Route{
		Brokers:   []pb.BrokerSpec_ID{{"zone-a", "member-1"}},
		Endpoints: []pb.Endpoint{pb.Endpoint("http://" + peer1.addr() + "/path")},
	}
	_, err = d.dial(ctx, pb.BrokerSpec_ID{Zone: "zone-b", Suffix: "member-2"}, route)
	c.Check(err, gc.ErrorMatches, `no such Broker in Route \(id: zone:"zone-b" suffix:"member-2" .*\)`)

	// Also expect an error if Endpoints are not attached.
	route.Endpoints = nil
	_, err = d.dial(ctx, pb.BrokerSpec_ID{Zone: "zone-a", Suffix: "member-1"}, route)
	c.Check(err, gc.ErrorMatches, `missing Route Endpoints \(id: zone:"zone-a" suffix:"member-1" .*\)`)
}

var _ = gc.Suite(&DialerSuite{})