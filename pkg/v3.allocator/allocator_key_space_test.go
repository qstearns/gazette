package v3_allocator

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/LiveRamp/gazette/pkg/keyspace"
	"github.com/coreos/etcd/clientv3"
	"github.com/coreos/etcd/mvcc/mvccpb"
	gc "github.com/go-check/check"
)

type AllocKeySpaceSuite struct{}

func (s *AllocKeySpaceSuite) TestAllocKeySpaceDecoding(c *gc.C) {
	var client = etcdCluster.RandClient()
	var ctx = context.Background()
	buildAllocKeySpaceFixture(c, ctx, client)

	var ks = NewAllocatorKeySpace("/root", testAllocDecoder{})
	c.Check(ks.Load(ctx, client, 0), gc.IsNil)

	// Verify expected contents of the decoded KeySpace.
	var expect = []struct {
		key   string
		value interface{}
	}{
		{"/root/assign/item-1/us-east/foo/1", Assignment{
			ItemID: "item-1", MemberZone: "us-east", MemberSuffix: "foo", Slot: 1, AssignmentValue: testAssignment{true}}},
		{"/root/assign/item-1/us-west/baz/0", Assignment{
			ItemID: "item-1", MemberZone: "us-west", MemberSuffix: "baz", Slot: 0, AssignmentValue: testAssignment{true}}},
		{"/root/assign/item-missing/us-west/baz/0", Assignment{
			ItemID: "item-missing", MemberZone: "us-west", MemberSuffix: "baz", Slot: 0, AssignmentValue: testAssignment{false}}},
		{"/root/assign/item-two/missing/member/2", Assignment{
			ItemID: "item-two", MemberZone: "missing", MemberSuffix: "member", Slot: 2, AssignmentValue: testAssignment{false}}},
		{"/root/assign/item-two/us-east/bar/0", Assignment{
			ItemID: "item-two", MemberZone: "us-east", MemberSuffix: "bar", Slot: 0, AssignmentValue: testAssignment{true}}},
		{"/root/assign/item-two/us-west/baz/1", Assignment{
			ItemID: "item-two", MemberZone: "us-west", MemberSuffix: "baz", Slot: 1, AssignmentValue: testAssignment{false}}},

		{"/root/items/item-1", Item{ID: "item-1", ItemValue: testItem{R: 2}}},
		{"/root/items/item-two", Item{ID: "item-two", ItemValue: testItem{R: 1}}},

		{"/root/members/us-east/bar", Member{Zone: "us-east", Suffix: "bar", MemberValue: testMember{R: 1}}},
		{"/root/members/us-east/foo", Member{Zone: "us-east", Suffix: "foo", MemberValue: testMember{R: 2}}},
		{"/root/members/us-west/baz", Member{Zone: "us-west", Suffix: "baz", MemberValue: testMember{R: 3}}},
	}

	c.Assert(ks.KeyValues, gc.HasLen, len(expect))
	for i, e := range expect {
		c.Check(string(ks.KeyValues[i].Raw.Key), gc.Equals, e.key)
		c.Check(ks.KeyValues[i].Decoded, gc.DeepEquals, e.value)
	}
}

func (s *AllocKeySpaceSuite) TestAssignmentCompare(c *gc.C) {
	var client = etcdCluster.RandClient()
	var ctx = context.Background()
	buildAllocKeySpaceFixture(c, ctx, client)

	var ks = NewAllocatorKeySpace("/root", testAllocDecoder{})
	c.Check(ks.Load(ctx, client, 0), gc.IsNil)

	// Expect |compareAssignment| matches natural Assignment key order.
	var assignments = ks.Prefixed(ks.Root + AssignmentsPrefix)
	for i := range assignments[:len(assignments)-1] {
		var ai, aii = assignmentAt(assignments, i), assignmentAt(assignments, i+1)
		c.Check(compareAssignment(ai, aii), gc.Equals, -1)
		c.Check(compareAssignment(aii, ai), gc.Equals, 1)
		c.Check(compareAssignment(ai, ai), gc.Equals, 0)
	}

	// Verify ItemID, MemberZone, and MemberSuffix drive the comparision, in that order.
	var a1 = Assignment{ItemID: "aaa", MemberZone: "bbb", MemberSuffix: "ccc"}
	var a2 = a1

	c.Check(compareAssignment(a1, a2), gc.Equals, 0)
	c.Check(compareAssignment(a2, a1), gc.Equals, 0)

	a2.MemberSuffix = "ccd"
	c.Check(compareAssignment(a2, a1), gc.Equals, 1)
	c.Check(compareAssignment(a1, a2), gc.Equals, -1)

	a2.MemberZone = "bba"
	c.Check(compareAssignment(a2, a1), gc.Equals, -1)
	c.Check(compareAssignment(a1, a2), gc.Equals, 1)

	a2.ItemID = "aab"
	c.Check(compareAssignment(a2, a1), gc.Equals, 1)
	c.Check(compareAssignment(a1, a2), gc.Equals, -1)
}

func (s *AllocKeySpaceSuite) TestLeftJoin(c *gc.C) {
	var L1 = []float32{
		1.01, // 0
		1.05, // 1
		3.00, // 2
		4.04, // 3
		4.05, // 4
		5.02, // 5
		7.01, // 6
		9.08, // 7
		9.09, // 8
	}
	var L2 = []float32{
		1.3, // 0
		4.5, // 1
		4.7, // 2
		4.9, // 3
		5.1, // 4
		5.2, // 5
		6.1, // 6
		6.2, // 7
		7.6, // 8
	}
	// Compare the integer portions of L1, L2 items.
	var compare = func(l, r int) int {
		switch li, ri := int(L1[l]), int(L2[r]); {
		case li < ri:
			return -1
		case li == ri:
			return 0
		default:
			return 1
		}
	}

	var expect = []cursor{
		// 1.01, 1.05 <=> 1.3
		{left: 0, rightBegin: 0, rightEnd: 1},
		{left: 1, rightBegin: 0, rightEnd: 1},
		// 3.00 <=> empty
		{left: 2, rightBegin: 1, rightEnd: 1},
		// 4.04, 4.05 <=> 4.5, 4.7, 4.9
		{left: 3, rightBegin: 1, rightEnd: 4},
		{left: 4, rightBegin: 1, rightEnd: 4},
		// 5.02 <=> 5.1, 5.2
		{left: 5, rightBegin: 4, rightEnd: 6},
		// 7.01 <=> 7.6
		{left: 6, rightBegin: 8, rightEnd: 9},
		// 9.08, 9.09 <=> empty
		{left: 7, rightBegin: 9, rightEnd: 9},
		{left: 8, rightBegin: 9, rightEnd: 9},
	}

	var lj = leftJoin{
		lenL:    len(L1),
		lenR:    len(L2),
		compare: compare,
	}
	for _, tc := range expect {
		var cur, ok = lj.next()
		c.Check(ok, gc.Equals, true)
		c.Check(cur, gc.Equals, tc)
	}
	var _, ok = lj.next()
	c.Check(ok, gc.Equals, false)

	// Run again, this time reverse L1/L2 left/right side order.

	expect = []cursor{
		// 1.3 <=> 1.01, 1.05
		{left: 0, rightBegin: 0, rightEnd: 2},
		// 4.5, 4.7, 4.9 <=> 4.04, 4.05
		{left: 1, rightBegin: 3, rightEnd: 5},
		{left: 2, rightBegin: 3, rightEnd: 5},
		{left: 3, rightBegin: 3, rightEnd: 5},
		// 5.1, 5.2 <=> 5.02
		{left: 4, rightBegin: 5, rightEnd: 6},
		{left: 5, rightBegin: 5, rightEnd: 6},
		// 6.1, 6.2 <=> empty
		{left: 6, rightBegin: 6, rightEnd: 6},
		{left: 7, rightBegin: 6, rightEnd: 6},
		// 7.6 <=> empty
		{left: 8, rightBegin: 6, rightEnd: 7},
	}

	lj = leftJoin{
		lenL:    len(L2),
		lenR:    len(L1),
		compare: func(l, r int) int { return -1 * compare(r, l) },
	}
	for _, tc := range expect {
		var cur, ok = lj.next()
		c.Check(ok, gc.Equals, true)
		c.Check(cur, gc.Equals, tc)
	}
	_, ok = lj.next()
	c.Check(ok, gc.Equals, false)
}

func buildAllocKeySpaceFixture(c *gc.C, ctx context.Context, client *clientv3.Client) {
	var _, err = client.Delete(ctx, "", clientv3.WithPrefix())
	c.Assert(err, gc.IsNil)

	for k, v := range map[string]string{
		"/root/items/item-1":   `{"R": 2}`, // Items index 0
		"/root/items/item-two": `{"R": 1}`, // 1

		// Invalid KeyValues which are omitted.
		"/root/items/invalid/item/key": `{"R": 1}`,
		"/root/items/valid-id":         `invalid value`,

		"/root/members/us-east/bar": `{"R": 1}`, // Members index 0
		"/root/members/us-east/foo": `{"R": 2}`, // 1
		"/root/members/us-west/baz": `{"R": 3}`, // 2.

		// Invalid KeyValues which are omitted.
		"/root/members/invalid-key":       `{"R": 1}`,
		"/root/members/us-west/valid-key": `invalid value`,

		"/root/assign/item-1/us-east/foo/1":       `consistent`, // Assignments index 0
		"/root/assign/item-1/us-west/baz/0":       `consistent`, // 1
		"/root/assign/item-missing/us-west/baz/0": ``,           // 2
		"/root/assign/item-two/missing/member/2":  ``,           // 3
		"/root/assign/item-two/us-east/bar/0":     `consistent`, // 4
		"/root/assign/item-two/us-west/baz/1":     ``,           // 5

		// Invalid KeyValues which are omitted.
		"/root/assign/item-1/us-east/foo/invalid-slot": ``,
		"/root/assign/item-two/valid/key/2":            `invalid value`,

		"/root/aaaaa/unknown/prefix": ``,
		"/root/jjjjj/unknown/prefix": ``,
		"/root/zzzzz/unknown/prefix": ``,
	} {
		var _, err = client.Put(ctx, k, v)
		c.Assert(err, gc.IsNil)
	}
}

type testAllocDecoder struct{}

func (d testAllocDecoder) DecodeItem(id string, raw *mvccpb.KeyValue) (ItemValue, error) {
	var i testItem
	var err = json.Unmarshal(raw.Value, &i)
	return i, err
}

func (d testAllocDecoder) DecodeMember(zone, suffix string, raw *mvccpb.KeyValue) (MemberValue, error) {
	var m testMember
	var err = json.Unmarshal(raw.Value, &m)
	return m, err
}

func (d testAllocDecoder) DecodeAssignment(itemID, zone, suffix string, slot int, raw *mvccpb.KeyValue) (AssignmentValue, error) {
	switch s := string(raw.Value); s {
	case "":
		return testAssignment{consistent: false}, nil
	case "consistent":
		return testAssignment{consistent: true}, nil
	default:
		return nil, fmt.Errorf("invalid value: %s", s)
	}
}

type testItem struct{ R int }

func (i testItem) DesiredReplication() int { return i.R }
func (i testItem) IsConsistent(assignment keyspace.KeyValue, allAssignments keyspace.KeyValues) bool {
	return assignment.Decoded.(Assignment).AssignmentValue.(testAssignment).consistent
}

type testMember struct{ R int }

func (m testMember) ItemLimit() int { return m.R }

type testAssignment struct{ consistent bool }

var _ = gc.Suite(&AllocKeySpaceSuite{})
