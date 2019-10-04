package tracker

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/rand"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	web3 "github.com/umbracle/go-web3"
	"github.com/umbracle/go-web3/abi"
	"github.com/umbracle/go-web3/jsonrpc"
	"github.com/umbracle/go-web3/testutil"
)

func testConfig() *Config {
	config := DefaultConfig()
	config.BatchSize = 10
	return config
}

func testTracker(t *testing.T, provider Provider, configFn func(t *Tracker)) *InmemStore {
	store := NewInmemStore()

	tt := NewTracker(provider, testConfig())
	tt.store = store

	if configFn != nil {
		configFn(tt)
	}

	if err := tt.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestPolling(t *testing.T) {
	s := testutil.NewTestServer(t, nil)
	defer s.Close()

	client, _ := jsonrpc.NewClient(s.HTTPAddr())

	config := DefaultConfig()
	config.PollInterval = 1 * time.Second

	c0 := &testutil.Contract{}
	c0.AddEvent(testutil.NewEvent("A").Add("uint256", true).Add("uint256", true))
	c0.EmitEvent("setA1", "A", "1", "2")

	_, addr0 := s.DeployContract(c0)

	// send 5 txns
	for i := 0; i < 5; i++ {
		s.TxnTo(addr0, "setA1")
	}

	eventCh := make(chan *Event, 1024)

	tt := NewTracker(client.Eth(), config)
	tt.store = NewInmemStore()
	tt.EventCh = eventCh

	if err := tt.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	// consume all the events from the sync process
	for {
		select {
		case <-eventCh:
		default:
			goto EXIT
		}
	}
EXIT:

	ctx, cancelFn := context.WithCancel(context.Background())
	defer cancelFn()

	tt.Polling(ctx)

	// send another 5 transactions, we have to have another log each time
	for i := 0; i < 5; i++ {
		receipt := s.TxnTo(addr0, "setA1")

		select {
		case evnt := <-eventCh:
			if !reflect.DeepEqual(evnt.AddedLogs, receipt.Logs) {
				t.Fatal("bad")
			}

		case <-time.After(2 * time.Second): // wait at least the polling interval
			t.Fatal("event expected")
		}
	}
}

func TestFilterIntegration(t *testing.T) {
	s := testutil.NewTestServer(t, nil)
	defer s.Close()

	client, _ := jsonrpc.NewClient(s.HTTPAddr())

	c0 := &testutil.Contract{}
	c0.AddEvent(testutil.NewEvent("A").Add("uint256", true).Add("uint256", true))
	c0.EmitEvent("setA1", "A", "1", "2")

	_, addr0 := s.DeployContract(c0)
	_, addr1 := s.DeployContract(c0)

	for i := 0; i < 20; i++ {
		if i%2 == 0 {
			s.TxnTo(addr0, "setA1")
		} else {
			s.TxnTo(addr1, "setA1")
		}
	}

	// sync all the logs
	store0 := testTracker(t, client.Eth(), nil)
	if len(store0.logs) != 20 {
		t.Fatal("bad")
	}

	// filter by address
	store1 := testTracker(t, client.Eth(), func(tt *Tracker) {
		tt.SetFilterAddress(addr0)
	})
	if len(store1.logs) != 10 {
		t.Fatal("bad")
	}

	// filter by value
	store2 := testTracker(t, client.Eth(), func(tt *Tracker) {
		typ, _ := abi.NewType("uint256")
		topic, _ := abi.EncodeTopic(typ, 1)

		tt.SetFilterTopics([]*web3.Hash{nil, &topic})
	})
	if len(store2.logs) != 20 {
		t.Fatal("bad")
	}
}

func TestFilterIntegrationEventHash(t *testing.T) {
	s := testutil.NewTestServer(t, nil)
	defer s.Close()

	client, _ := jsonrpc.NewClient(s.HTTPAddr())

	c0 := &testutil.Contract{}
	c0.AddEvent(testutil.NewEvent("A").Add("uint256", true).Add("uint256", true))
	c0.EmitEvent("setA1", "A", "1", "2")

	c1 := &testutil.Contract{}
	c1.AddEvent(testutil.NewEvent("B").Add("uint256", true).Add("uint256", true))
	c1.EmitEvent("setB1", "B", "1", "2")

	artifacts0, addr0 := s.DeployContract(c0)
	_, addr1 := s.DeployContract(c0)

	abi0, _ := abi.NewABI(artifacts0.Abi)

	for i := 0; i < 20; i++ {
		if i%2 == 0 {
			s.TxnTo(addr0, "setA1")
		} else {
			s.TxnTo(addr1, "setB1")
		}
	}

	eventTopicID := abi0.Events["A"].ID()

	store := testTracker(t, client.Eth(), func(t *Tracker) {
		t.SetFilterTopics([]*web3.Hash{&eventTopicID})
	})
	if len(store.logs) != 10 {
		t.Fatal("bad")
	}

	store1 := testTracker(t, client.Eth(), func(t *Tracker) {
		eventTopicID[1] = 1
		t.SetFilterTopics([]*web3.Hash{&eventTopicID})
	})
	if len(store1.logs) != 0 {
		t.Fatal("bad")
	}
}

func TestPreflight(t *testing.T) {
	store := NewInmemStore()

	l := mockList{}
	l.create(0, 100, func(b *mockBlock) {})

	m := &mockClient{}
	m.addScenario(l)

	tt0 := NewTracker(m, testConfig())
	tt0.store = store

	if err := tt0.preSyncCheck(); err != nil {
		t.Fatal(err)
	}

	l0 := mockList{}
	l0.create(0, 100, func(b *mockBlock) {
		b = b.Extra("1")
	})

	m.addScenario(l0)

	tt1 := NewTracker(m, testConfig())
	tt1.store = store

	if err := tt1.preSyncCheck(); err == nil {
		t.Fatal("it should fail")
	}
}

func TestTrackerSyncerRestarts(t *testing.T) {
	store := NewInmemStore()
	m := &mockClient{}
	l := mockList{}

	advance := func(first, last int, void ...bool) {
		if len(void) == 0 {
			l.create(first, last, func(b *mockBlock) {
				if b.num%5 == 0 {
					b = b.Log("0x1")
				}
			})
			m.addScenario(l)
		}

		tt := NewTracker(m, testConfig())
		tt.store = store

		if err := tt.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}

		if tt.blocks[0].Number != uint64(last-10) {
			t.Fatal("bad")
		}
		if tt.blocks[9].Number != uint64(last-1) {
			t.Fatal("bad")
		}
		if !compareLogs(l.GetLogs(), store.logs) {
			t.Fatal("bad")
		}
	}

	// initial range
	advance(0, 100)

	// dont advance
	advance(0, 100, true)

	// advance less than backlog
	advance(100, 105)

	// advance more than backlog
	advance(105, 150)
}

func testSyncerReconcile(t *testing.T, iniLen, forkNum, endLen int) {
	// test that the syncer can reconcile if there is a fork in the saved state
	l := mockList{}
	l.create(0, iniLen, func(b *mockBlock) {
		b = b.Log("0x01")
	})

	m := &mockClient{}
	m.addScenario(l)

	store := NewInmemStore()

	tt0 := NewTracker(m, testConfig())
	tt0.store = store

	if err := tt0.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}

	// create a fork at 'forkNum' and continue to 'endLen'
	l1 := mockList{}
	l1.create(0, endLen, func(b *mockBlock) {
		if b.num < forkNum {
			b = b.Log("0x01") // old fork
		} else {
			if b.num == forkNum {
				b = b.Log("0x02")
			} else {
				b = b.Log("0x03")
			}
			b = b.Extra("123") // used to set the new fork
		}
	})

	m1 := &mockClient{}
	m1.addScenario(l)
	m1.addScenario(l1)

	tt1 := NewTracker(m1, testConfig())
	tt1.store = store

	if err := tt1.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !compareLogs(l1.GetLogs(), store.logs) {
		t.Fatal("bad")
	}

	// check the content of the logs

	// first half
	for i := 0; i < forkNum; i++ {
		if store.logs[i].Data[0] != 0x1 {
			t.Fatal("bad")
		}
	}
	// fork point
	if store.logs[forkNum].Data[0] != 0x2 {
		t.Fatal("bad")
	}
	// second half
	for i := forkNum + 1; i < endLen; i++ {
		if store.logs[i].Data[0] != 0x3 {
			t.Fatal("bad")
		}
	}
}

func TestTrackerSyncerReconcile(t *testing.T) {
	t.Run("Backlog", func(t *testing.T) {
		testSyncerReconcile(t, 50, 45, 55)
	})
	t.Run("Historical", func(t *testing.T) {
		testSyncerReconcile(t, 50, 45, 100)
	})
}

func randomInt(min, max int) int {
	return min + rand.Intn(max-min)
}

func testTrackerSyncerRandom(t *testing.T, n int, backlog uint64) {
	m := &mockClient{}
	c := 0 // current block
	f := 0 // current fork

	store := NewInmemStore()

	config := testConfig()
	config.MaxBlockBacklog = backlog

	for i := 0; i < n; i++ {
		fmt.Printf("ITER: %d\n", i)

		// create the new batch of blocks
		var forkSize int
		if randomInt(0, 10) < 3 && c > 10 {
			// add a fork, go back at most maxBacklogSize
			forkSize = randomInt(1, int(config.MaxBlockBacklog))
			c = c - forkSize
			f++
		}

		forkID := strconv.Itoa(f)

		// add new blocks
		l := mockList{}

		// we have to create at least the blocks removed by the fork, otherwise
		// we may end up going backwards if the forks remove more data than the
		// advance includes

		start := forkSize
		if start == 0 && i == 0 {
			start = 1 // at least advance one block on the first iteration
		}
		num := randomInt(start, 20)
		count := 0

		for j := c; j < c+num; j++ {
			bb := mock(j).Extra(forkID)
			if j != 0 {
				count++
				bb = bb.Log(forkID)
			}
			l = append(l, bb)
		}

		m.addScenario(l)

		eventCh := make(chan *Event, 1024)

		tt := NewTracker(m, config)
		tt.store = store
		tt.EventCh = eventCh

		if err := tt.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}

		var added, removed []*web3.Log
		for {
			select {
			case evnt := <-eventCh:
				added = append(added, evnt.AddedLogs...)
				removed = append(removed, evnt.RemovedLogs...)

			default:
				// no more events to read
				goto EXIT
			}
		}
	EXIT:

		// validate the included logs
		if len(added) != count {
			t.Fatal("bad added logs")
		}
		// validate the removed logs
		if len(removed) != forkSize {
			t.Fatal("bad removed logs")
		}

		// validate blocks
		if blocks := m.getLastBlocks(config.MaxBlockBacklog); !compareBlocks(tt.blocks, blocks) {
			// tracker does not consider block 0 but getLastBlocks does return it, this is only a problem
			// with syncs on chains lower than maxBacklog
			if !compareBlocks(blocks[1:], tt.blocks) {
				t.Fatal("bad blocks")
			}
		}
		// validate logs
		if logs := m.getAllLogs(); !compareLogs(store.logs, logs) {
			t.Fatal("bad logs")
		}

		c += num
	}
}

func TestTrackerSyncerRandom(t *testing.T) {
	rand.Seed(time.Now().UTC().UnixNano())

	for i := 0; i < 100; i++ {
		t.Run("", func(t *testing.T) {
			testTrackerSyncerRandom(t, 100, uint64(randomInt(2, 10)))
		})
	}
}

type mockCall int

const (
	blockByNumberCall mockCall = iota
	blockByHashCall
	blockNumberCall
	getLogsCall
)

type mockClient struct {
	lock     sync.Mutex
	num      uint64
	blockNum map[uint64]web3.Hash
	blocks   map[web3.Hash]*web3.Block
	logs     map[web3.Hash][]*web3.Log
}

func (d *mockClient) getLastBlocks(n uint64) (res []*web3.Block) {
	if d.num == 0 {
		return
	}
	num := n
	if d.num < num {
		num = d.num + 1
	}

	for i := int(num - 1); i >= 0; i-- {
		res = append(res, d.blocks[d.blockNum[d.num-uint64(i)]])
	}
	return
}

func (d *mockClient) getAllLogs() (res []*web3.Log) {
	if d.num == 0 {
		return
	}
	for i := uint64(0); i <= d.num; i++ {
		res = append(res, d.logs[d.blocks[d.blockNum[i]].Hash]...)
	}
	return
}

func (d *mockClient) addScenario(m mockList) {
	d.lock.Lock()
	defer d.lock.Unlock()

	// add the logs
	for _, b := range m {
		block := &web3.Block{
			Hash:   b.Hash(),
			Number: uint64(b.num),
		}

		if b.num != 0 {
			bb, err := d.blockByNumberLock(uint64(b.num) - 1)
			if err != nil {
				// This happens during reconcile tests because we include only partial blocks
				block.ParentHash = encodeHash(strconv.Itoa(b.num - 1))
			} else {
				block.ParentHash = bb.Hash
			}
		}

		// add history block
		d.addBlocks(block)

		// add logs
		// remove any other logs for this block in case there are any
		if _, ok := d.logs[block.Hash]; ok {
			delete(d.logs, block.Hash)
		}

		d.addLogs(b.GetLogs())
	}
}

func (d *mockClient) addLogs(logs []*web3.Log) {
	if d.logs == nil {
		d.logs = map[web3.Hash][]*web3.Log{}
	}
	for _, log := range logs {
		entry, ok := d.logs[log.BlockHash]
		if ok {
			entry = append(entry, log)
		} else {
			entry = []*web3.Log{log}
		}
		d.logs[log.BlockHash] = entry
	}
}

func (d *mockClient) addBlocks(bb ...*web3.Block) {
	if d.blocks == nil {
		d.blocks = map[web3.Hash]*web3.Block{}
	}
	if d.blockNum == nil {
		d.blockNum = map[uint64]web3.Hash{}
	}
	for _, b := range bb {
		if b.Number > d.num {
			d.num = b.Number
		}
		d.blocks[b.Hash] = b
		d.blockNum[b.Number] = b.Hash
	}
}

func (d *mockClient) BlockNumber() (uint64, error) {
	d.lock.Lock()
	defer d.lock.Unlock()

	return d.num, nil
}

func (d *mockClient) GetBlockByHash(hash web3.Hash, full bool) (*web3.Block, error) {
	d.lock.Lock()
	defer d.lock.Unlock()

	b := d.blocks[hash]
	if b == nil {
		return nil, fmt.Errorf("hash %s not found", hash)
	}
	return b, nil
}

func (d *mockClient) blockByNumberLock(i uint64) (*web3.Block, error) {
	hash, ok := d.blockNum[i]
	if !ok {
		return nil, fmt.Errorf("number %d not found", i)
	}
	return d.blocks[hash], nil
}

func (d *mockClient) GetBlockByNumber(i web3.BlockNumber, full bool) (*web3.Block, error) {
	d.lock.Lock()
	defer d.lock.Unlock()

	if i < 0 {
		return nil, fmt.Errorf("not supported")
	}
	return d.blockByNumberLock(uint64(i))
}

func (d *mockClient) GetLogs(filter *web3.LogFilter) ([]*web3.Log, error) {
	d.lock.Lock()
	defer d.lock.Unlock()

	if filter.BlockHash != nil {
		return d.logs[*filter.BlockHash], nil
	}

	from, to := uint64(*filter.From), uint64(*filter.To)
	if from > to {
		return nil, fmt.Errorf("from higher than to")
	}
	if int(to) > len(d.blocks) {
		return nil, fmt.Errorf("out of bounds")
	}

	logs := []*web3.Log{}
	for i := from; i <= to; i++ {
		b, err := d.blockByNumberLock(i)
		if err != nil {
			return nil, err
		}
		elems, ok := d.logs[b.Hash]
		if ok {
			logs = append(logs, elems...)
		}
	}
	return logs, nil
}

type mockLog struct {
	data string
}

type mockBlock struct {
	hash   string
	extra  string
	parent string
	num    int
	logs   []*mockLog
}

func mustDecodeHash(str string) []byte {
	if strings.HasPrefix(str, "0x") {
		str = str[2:]
	}
	if len(str)%2 == 1 {
		str = str + "0"
	}
	buf, err := hex.DecodeString(str)
	if err != nil {
		panic(err)
	}
	return buf
}

func (m *mockBlock) Extra(data string) *mockBlock {
	m.extra = data
	return m
}

func (m *mockBlock) GetLogs() (logs []*web3.Log) {
	for _, log := range m.logs {
		logs = append(logs, &web3.Log{Data: mustDecodeHash(log.data), BlockNumber: uint64(m.num), BlockHash: m.Hash()})
	}
	return
}

func (m *mockBlock) Log(data string) *mockBlock {
	m.logs = append(m.logs, &mockLog{data})
	return m
}

func (m *mockBlock) Num(i int) *mockBlock {
	m.num = i
	return m
}

func (m *mockBlock) Parent(i int) *mockBlock {
	m.parent = strconv.Itoa(i)
	m.num = i + 1
	return m
}

func encodeHash(str string) (h web3.Hash) {
	tmp := ""
	for i := 0; i < 64-len(str); i++ {
		tmp += "0"
	}
	str = "0x" + tmp + str
	if err := h.UnmarshalText([]byte(str)); err != nil {
		panic(err)
	}
	return
}

func (m *mockBlock) Hash() web3.Hash {
	return encodeHash(m.extra + m.hash)
}

func (m *mockBlock) Block() *web3.Block {
	b := &web3.Block{
		Hash:   m.Hash(),
		Number: uint64(m.num),
	}
	if m.num != 0 {
		b.ParentHash = encodeHash(m.parent)
	}
	return b
}

func mock(number int) *mockBlock {
	return &mockBlock{hash: strconv.Itoa(number), num: number, parent: strconv.Itoa(number - 1)}
}

type mockList []*mockBlock

func (m *mockList) create(from, to int, callback func(b *mockBlock)) {
	for i := from; i < to; i++ {
		b := mock(i)
		callback(b)
		*m = append(*m, b)
	}
}

func (m *mockList) GetLogs() (res []*web3.Log) {
	for _, log := range *m {
		res = append(res, log.GetLogs()...)
	}
	return
}

func (m *mockList) ToBlocks() []*web3.Block {
	e := []*web3.Block{}
	for _, i := range *m {
		e = append(e, i.Block())
	}
	return e
}

func TestTrackerReconcile(t *testing.T) {
	type Event struct {
		Added   mockList
		Removed mockList
	}

	type Reconcile struct {
		block *mockBlock
		event *Event
	}

	cases := []struct {
		Name      string
		Scenario  mockList
		History   mockList
		Reconcile []Reconcile
		Expected  mockList
	}{
		{
			Name: "Empty history",
			Reconcile: []Reconcile{
				{
					block: mock(0x1).Log("0x1"),
					event: &Event{
						Added: mockList{
							mock(0x1).Log("0x1"),
						},
					},
				},
			},
			Expected: []*mockBlock{
				mock(1).Log("0x1"),
			},
		},
		{
			Name: "Repeated header",
			History: []*mockBlock{
				mock(0x1),
			},
			Reconcile: []Reconcile{
				{
					block: mock(0x1),
				},
			},
			Expected: []*mockBlock{
				mock(0x1),
			},
		},
		{
			Name: "New head",
			History: mockList{
				mock(0x1),
			},
			Reconcile: []Reconcile{
				{
					block: mock(0x2),
					event: &Event{
						Added: mockList{
							mock(0x2),
						},
					},
				},
			},
			Expected: mockList{
				mock(0x1),
				mock(0x2),
			},
		},
		{
			Name: "Ignore block already on history",
			History: mockList{
				mock(0x1),
				mock(0x2),
				mock(0x3),
			},
			Reconcile: []Reconcile{
				{
					block: mock(0x2),
				},
			},
			Expected: mockList{
				mock(0x1),
				mock(0x2),
				mock(0x3),
			},
		},
		{
			Name: "Multi Roll back",
			History: mockList{
				mock(0x1),
				mock(0x2),
				mock(0x3).Log("0x3"),
				mock(0x4).Log("0x4"),
			},
			Reconcile: []Reconcile{
				{
					block: mock(0x30).Parent(0x2).Log("0x30"),
					event: &Event{
						Added: mockList{
							mock(0x30).Parent(0x2).Log("0x30"),
						},
						Removed: mockList{
							mock(0x3).Log("0x3"),
							mock(0x4).Log("0x4"),
						},
					},
				},
			},
			Expected: mockList{
				mock(0x1),
				mock(0x2),
				mock(0x30).Parent(0x2).Log("0x30"),
			},
		},
		{
			Name: "Backfills missing blocks",
			Scenario: mockList{
				mock(0x3),
				mock(0x4).Log("0x2"),
			},
			History: mockList{
				mock(0x1).Log("0x1"),
				mock(0x2),
			},
			Reconcile: []Reconcile{
				{
					block: mock(0x5).Log("0x3"),
					event: &Event{
						Added: mockList{
							mock(0x3),
							mock(0x4).Log("0x2"),
							mock(0x5).Log("0x3"),
						},
					},
				},
			},
			Expected: mockList{
				mock(0x1).Log("0x1"),
				mock(0x2),
				mock(0x3),
				mock(0x4).Log("0x2"),
				mock(0x5).Log("0x3"),
			},
		},
		{
			Name: "Rolls back and backfills",
			Scenario: mockList{
				mock(0x30).Parent(0x2).Num(3).Log("0x5"),
				mock(0x40).Parent(0x30).Num(4),
			},
			History: mockList{
				mock(0x1),
				mock(0x2).Log("0x3"),
				mock(0x3).Log("0x2"),
				mock(0x4).Log("0x1"),
			},
			Reconcile: []Reconcile{
				{
					block: mock(0x50).Parent(0x40).Num(5),
					event: &Event{
						Added: mockList{
							mock(0x30).Parent(0x2).Num(3).Log("0x5"),
							mock(0x40).Parent(0x30).Num(4),
							mock(0x50).Parent(0x40).Num(5),
						},
						Removed: mockList{
							mock(0x3).Log("0x2"),
							mock(0x4).Log("0x1"),
						},
					},
				},
			},
			Expected: mockList{
				mock(0x1),
				mock(0x2).Log("0x3"),
				mock(0x30).Parent(0x2).Num(3).Log("0x5"),
				mock(0x40).Parent(0x30).Num(4),
				mock(0x50).Parent(0x40).Num(5),
			},
		},
	}

	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			// safe check for now, we ma need to restart the tracker and mock client for every reconcile scenario?
			if len(c.Reconcile) != 1 {
				t.Fatal("only one reconcile supported so far")
			}

			m := &mockClient{}

			// add the full scenario with the logs
			m.addScenario(c.Scenario)

			// add the logs of the reconcile block because those are also unknown for the tracker
			m.addLogs(c.Reconcile[0].block.GetLogs())

			store := NewInmemStore()

			tt := NewTracker(m, DefaultConfig())
			tt.store = store

			// build past block history
			for _, b := range c.History.ToBlocks() {
				tt.addBlock(b)
			}
			// add the history to the store
			for _, b := range c.History {
				tt.store.StoreLogs(b.GetLogs())
			}

			for _, b := range c.Reconcile {
				evnt, err := tt.handleReconcile(b.block.Block())
				if err != nil {
					t.Fatal(err)
				}

				if b.event == nil {
					continue
				}

				// check logs
				if !compareLogs(b.event.Added.GetLogs(), evnt.AddedLogs) {
					t.Fatal("err")
				}
				if !compareLogs(b.event.Removed.GetLogs(), evnt.RemovedLogs) {
					t.Fatal("err")
				}

				// check blocks
				if !compareBlocks(b.event.Added.ToBlocks(), evnt.Added) {
					t.Fatal("err")
				}
				if !compareBlocks(b.event.Removed.ToBlocks(), evnt.Removed) {
					t.Fatal("err")
				}
			}

			// check the post state (logs and blocks) after all the reconcile events
			if !compareLogs(store.logs, c.Expected.GetLogs()) {
				t.Fatal("bad3")
			}
			if !compareBlocks(tt.blocks, c.Expected.ToBlocks()) {
				t.Fatal("bad")
			}
		})
	}
}

func compareLogs(one, two []*web3.Log) bool {
	if len(one) != len(two) {
		return false
	}
	if len(one) == 0 {
		return true
	}
	return reflect.DeepEqual(one, two)
}

func compareBlocks(one, two []*web3.Block) bool {
	if len(one) != len(two) {
		return false
	}
	if len(one) == 0 {
		return true
	}
	return reflect.DeepEqual(one, two)
}
