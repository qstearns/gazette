package broker

import (
	"context"
	"fmt"
	"time"

	"github.com/hashicorp/golang-lru"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"

	pb "github.com/LiveRamp/gazette/pkg/protocol"
)

type dialer struct {
	cache *lru.Cache
}

// newDialer builds and returns a dialer.
func newDialer(size int) (dialer, error) {
	var cache, err = lru.NewWithEvict(size, func(key, value interface{}) {
		if err := value.(*grpc.ClientConn).Close(); err != nil {
			log.WithFields(log.Fields{"broker": key, "err": err}).
				Warn("failed to Close evicted grpc.ClientConn")
		}
	})
	return dialer{cache: cache}, err
}

func (d dialer) dial(ctx context.Context, id pb.BrokerSpec_ID, route pb.Route) (*grpc.ClientConn, error) {
	var ind int
	for ind = 0; ind != len(route.Brokers) && route.Brokers[ind] != id; ind++ {
	}

	if ind == len(route.Brokers) {
		return nil, fmt.Errorf("no such Broker in Route (id: %s, route: %s)", id.String(), route.String())
	} else if len(route.Endpoints) != len(route.Brokers) {
		return nil, fmt.Errorf("missing Route Endpoints (id: %s, route: %s)", id.String(), route.String())
	}

	// We perform the cache check explicitly _after_ examining Route, to prevent
	// development errors which appear as transient bugs due to caching effects.
	if v, ok := d.cache.Get(id); ok {
		return v.(*grpc.ClientConn), nil
	}

	var conn, err = grpc.DialContext(ctx, route.Endpoints[ind].URL().Host,
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: time.Second * 30}),
		grpc.WithInsecure(),
	)
	if err == nil {
		d.cache.Add(id, conn)
	}
	return conn, err
}