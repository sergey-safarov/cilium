// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package bpf

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"github.com/cilium/hive/hivetest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/testutils"
)

// Configure a generous timeout to prevent flakes when running in a noisy CI environment.
const (
	tick    = 100 * time.Millisecond
	timeout = 10 * time.Second
)

type TestKey struct {
	Key uint32
}
type TestLPMKey struct {
	PrefixLen uint32
	Key       uint32
}
type TestValue struct {
	Value uint32
}
type TestValues []TestValue

func (k *TestKey) String() string { return fmt.Sprintf("key=%d", k.Key) }
func (k *TestKey) New() MapKey    { return &TestKey{} }

func (k *TestLPMKey) String() string { return fmt.Sprintf("len=%d, key=%d", k.PrefixLen, k.Key) }
func (k *TestLPMKey) New() MapKey    { return &TestLPMKey{} }

func (v *TestValue) String() string { return fmt.Sprintf("value=%d", v.Value) }
func (v *TestValue) New() MapValue  { return &TestValue{} }
func (k *TestValue) NewSlice() any  { return &TestValues{} }

func setup(tb testing.TB) *Map {
	testutils.PrivilegedTest(tb)

	CheckOrMountFS(hivetest.Logger(tb), "")

	err := rlimit.RemoveMemlock()
	require.NoError(tb, err)

	testMap := NewMap("cilium_test",
		ebpf.Hash,
		&TestKey{},
		&TestValue{},
		maxEntries,
		unix.BPF_F_NO_PREALLOC,
	).WithCache()

	err = testMap.OpenOrCreate()
	require.NoError(tb, err, "Failed to create map")

	tb.Cleanup(func() {
		require.NoError(tb, testMap.Close())
	})

	return testMap
}

func setupPerCPU(tb testing.TB) *Map {
	testutils.PrivilegedTest(tb)

	CheckOrMountFS(hivetest.Logger(tb), "")

	err := rlimit.RemoveMemlock()
	require.NoError(tb, err)

	testMap := NewMap("cilium_test_percpu",
		ebpf.PerCPUArray,
		&TestKey{},
		&TestValue{},
		3,
		0,
	)

	err = testMap.OpenOrCreate()
	require.NoError(tb, err, "Failed to create map")

	tb.Cleanup(func() {
		require.NoError(tb, testMap.Close())
	})

	return testMap
}

var (
	maxEntries = 16
)

func mapsEqual(a, b *Map) bool {
	return a.name == b.name &&
		reflect.DeepEqual(a.spec, b.spec)
}

func TestPrivilegedOpen(t *testing.T) {
	setup(t)

	// Ensure that os.IsNotExist() can be used with Map.Open()
	noSuchMap := NewMap("cilium_test_no_exist",
		ebpf.Hash, &TestKey{}, &TestValue{}, maxEntries, 0)
	err := noSuchMap.Open()
	require.ErrorIs(t, err, os.ErrNotExist)

	// existingMap is the same as testMap. Opening should succeed.
	existingMap := NewMap("cilium_test",
		ebpf.Hash,
		&TestKey{},
		&TestValue{},
		maxEntries,
		unix.BPF_F_NO_PREALLOC).WithCache()
	defer func() {
		err = existingMap.Close()
		require.NoError(t, err)
	}()

	err = existingMap.Open()
	require.NoError(t, err)
	err = existingMap.Open()
	require.NoError(t, err)
}

func TestPrivilegedOpenMap(t *testing.T) {
	testMap := setup(t)
	logger := hivetest.Logger(t)

	openedMap, err := OpenMap("cilium_test_no_exist", &TestKey{}, &TestValue{})
	require.Error(t, err)
	require.Nil(t, openedMap)

	openedMap, err = OpenMap(MapPath(logger, "cilium_test"), &TestKey{}, &TestValue{})
	require.NoError(t, err)
	require.True(t, mapsEqual(openedMap, testMap))
}

func TestPrivilegedOpenOrCreate(t *testing.T) {
	setup(t)

	// existingMap is the same as testMap. OpenOrCreate should skip recreation.
	existingMap := NewMap("cilium_test",
		ebpf.Hash,
		&TestKey{},
		&TestValue{},
		maxEntries,
		unix.BPF_F_NO_PREALLOC).WithCache()
	err := existingMap.OpenOrCreate()
	require.NoError(t, err)

	// preallocMap unsets unix.BPF_F_NO_PREALLOC. OpenOrCreate should recreate map.
	EnableMapPreAllocation() // prealloc on/off is controllable in HASH map case.
	preallocMap := NewMap("cilium_test",
		ebpf.Hash,
		&TestKey{},
		&TestValue{},
		maxEntries,
		0).WithCache()
	err = preallocMap.OpenOrCreate()
	defer preallocMap.Close()
	require.NoError(t, err)
	DisableMapPreAllocation()

	// preallocMap is already open. OpenOrCreate does nothing.
	err = preallocMap.OpenOrCreate()
	require.NoError(t, err)
}

func TestPrivilegedRecreateMap(t *testing.T) {
	testMap := setup(t)

	parallelMap := NewMap("cilium_test",
		ebpf.Hash,
		&TestKey{},
		&TestValue{},
		maxEntries,
		unix.BPF_F_NO_PREALLOC).WithCache()
	err := parallelMap.Recreate()
	defer parallelMap.Close()
	require.NoError(t, err)

	err = parallelMap.Recreate()
	require.Error(t, err)

	// Check OpenMap warning section
	require.True(t, mapsEqual(parallelMap, testMap))

	key1 := &TestKey{Key: 101}
	value1 := &TestValue{Value: 201}
	key2 := &TestKey{Key: 102}
	value2 := &TestValue{Value: 202}

	err = testMap.Update(key1, value1)
	require.NoError(t, err)
	err = parallelMap.Update(key2, value2)
	require.NoError(t, err)

	value, err := testMap.Lookup(key1)
	require.NoError(t, err)
	require.EqualValues(t, value, value1)
	value, err = testMap.Lookup(key2)
	require.Error(t, err)
	require.Nil(t, value)

	value, err = parallelMap.Lookup(key1)
	require.Error(t, err)
	require.Nil(t, value)
	value, err = parallelMap.Lookup(key2)
	require.NoError(t, err)
	require.EqualValues(t, value, value2)
}

func TestPrivilegedBasicManipulation(t *testing.T) {
	setup(t)
	// existingMap is the same as testMap. Opening should succeed.
	existingMap := NewMap("cilium_test",
		ebpf.Hash,
		&TestKey{},
		&TestValue{},
		maxEntries,
		unix.BPF_F_NO_PREALLOC).
		WithCache().
		WithEvents(option.BPFEventBufferConfig{Enabled: true, MaxSize: 10})

	err := existingMap.Open()
	defer existingMap.Close()
	require.NoError(t, err)

	key1 := &TestKey{Key: 103}
	value1 := &TestValue{Value: 203}
	key2 := &TestKey{Key: 104}
	value2 := &TestValue{Value: 204}

	dumpEvents := func() []*Event {
		es := []*Event{}
		existingMap.DumpAndSubscribe(func(e *Event) {
			es = append(es, e)
		}, false)
		return es
	}
	event := func(i int) *Event {
		es := dumpEvents()
		if i >= len(es) {
			return nil
		}
		return dumpEvents()[i]
	}
	assertEvent := func(i int, key, value, desiredAction, action string) {
		e := event(i)
		if e.cacheEntry.Key != nil {
			require.Equal(t, key, e.cacheEntry.Key.String())
		}
		require.Equal(t, e.GetValue(), value)
		require.Equal(t, e.cacheEntry.DesiredAction.String(), desiredAction)
		require.Equal(t, e.GetAction(), action)
	}

	// event buffer should be empty
	require.Equal(t, 0, existingMap.events.buffer.Size())

	err = existingMap.Update(key1, value1)
	require.NoError(t, err)

	// Check events buffer
	require.Len(t, dumpEvents(), 1)
	require.Equal(t, "key=103", event(0).cacheEntry.Key.String())
	require.Equal(t, "value=203", event(0).cacheEntry.Value.String())

	// key    val
	// 103    203
	value, err := existingMap.Lookup(key1)
	require.NoError(t, err)
	require.EqualValues(t, value, value1)
	value, err = existingMap.Lookup(key2)
	require.Error(t, err)
	require.Nil(t, value)

	// Check events buffer, ensure it doesn't change.
	require.Len(t, dumpEvents(), 1)
	require.Equal(t, "key=103", event(0).cacheEntry.Key.String())
	require.Equal(t, "value=203", event(0).cacheEntry.Value.String())

	err = existingMap.Update(key1, value2)
	require.NoError(t, err)
	// key    val
	// 103    204
	value, err = existingMap.Lookup(key1)
	require.NoError(t, err)
	require.EqualValues(t, value, value2)

	// Check events buffer after second Update
	require.Len(t, dumpEvents(), 2)
	assertEvent(0, "key=103", "value=203", "sync", "update")
	require.Equal(t, "key=103", event(0).cacheEntry.Key.String())
	require.Equal(t, "value=203", event(0).cacheEntry.Value.String())
	require.Equal(t, "sync", event(0).cacheEntry.DesiredAction.String())
	require.Equal(t, "key=103", event(1).cacheEntry.Key.String()) // we used key1 again
	require.Equal(t, "value=204", event(1).cacheEntry.Value.String())
	require.Equal(t, "sync", event(1).cacheEntry.DesiredAction.String())

	err = existingMap.Update(key2, value2)
	require.NoError(t, err)
	// key    val
	// 103    204
	// 104    204
	value, err = existingMap.Lookup(key1)
	require.NoError(t, err)
	require.EqualValues(t, value, value2)
	value, err = existingMap.Lookup(key2)
	require.NoError(t, err)
	require.EqualValues(t, value, value2)

	require.Len(t, dumpEvents(), 3)
	assertEvent(0, "key=103", "value=203", "sync", "update")
	assertEvent(1, "key=103", "value=204", "sync", "update")
	assertEvent(2, "key=104", "value=204", "sync", "update")

	err = existingMap.Delete(key1)
	require.NoError(t, err)
	// key    val
	// 104    204
	value, err = existingMap.Lookup(key1)
	require.Error(t, err)
	require.Nil(t, value)

	err = existingMap.Delete(key1)
	require.Error(t, err)

	require.Len(t, dumpEvents(), 5)
	assertEvent(0, "key=103", "value=203", "sync", "update")
	assertEvent(1, "key=103", "value=204", "sync", "update")
	assertEvent(2, "key=104", "value=204", "sync", "update")
	assertEvent(3, "key=103", "<nil>", Delete.String(), "delete")
	assertEvent(4, "key=103", "<nil>", Delete.String(), "delete")

	require.NoError(t, event(3).GetLastError())
	require.Error(t, event(4).GetLastError())

	deleted, err := existingMap.SilentDelete(key1)
	require.NoError(t, err)
	require.False(t, deleted)

	require.Len(t, dumpEvents(), 6)
	assertEvent(5, "key=103", "<nil>", Delete.String(), "delete")
	require.NoError(t, event(5).GetLastError())

	err = existingMap.Update(key1, value1)
	require.NoError(t, err)

	require.Len(t, dumpEvents(), 7)
	assertEvent(6, "key=103", "value=203", OK.String(), "update")

	deleted, err = existingMap.SilentDelete(key1)
	require.NoError(t, err)
	require.True(t, deleted)

	require.Len(t, dumpEvents(), 8)
	assertEvent(7, "key=103", "<nil>", Delete.String(), "delete")

	value, err = existingMap.Lookup(key1)
	require.Error(t, err)
	require.Nil(t, value)

	err = existingMap.DeleteAll()
	require.NoError(t, err)
	value, err = existingMap.Lookup(key1)
	require.Error(t, err)
	require.Nil(t, value)
	value, err = existingMap.Lookup(key2)
	require.Error(t, err)
	require.Nil(t, value)

	require.Len(t, dumpEvents(), 9)
	assertEvent(8, "key=104", "<nil>", "sync", "delete-all")

	require.Equal(t, "key=103", event(0).cacheEntry.Key.String())
	require.Equal(t, "value=203", event(0).cacheEntry.Value.String())

	require.Equal(t, "key=103", event(1).cacheEntry.Key.String()) // we used key1 again

	err = existingMap.Update(key2, value2)
	require.NoError(t, err)
	require.Len(t, dumpEvents(), 10)
	assertEvent(9, "key=104", "value=204", OK.String(), "update")

	key3 := &TestKey{Key: 999}
	err = existingMap.Update(key3, value2)
	require.NoError(t, err)
	require.Len(t, dumpEvents(), 10) // full buffer
	assertEvent(0, "key=103", "value=204", OK.String(), "update")
	assertEvent(9, "key=999", "value=204", OK.String(), "update")

	key4 := &TestKey{Key: 1000}
	err = existingMap.Update(key4, value2)
	require.NoError(t, err)
	err = existingMap.DeleteAll()
	require.NoError(t, err)
	assertEvent(9, "<nil>", "<nil>", OK.String(), MapDeleteAll.String())

	// cleanup
	err = existingMap.DeleteAll()
	require.NoError(t, err)
}

func TestPrivilegedSubscribe(t *testing.T) {
	setup(t)

	existingMap := NewMap("cilium_test",
		ebpf.Hash,
		&TestKey{},
		&TestValue{},
		maxEntries,
		unix.BPF_F_NO_PREALLOC).
		WithCache().
		WithEvents(option.BPFEventBufferConfig{Enabled: true, MaxSize: 10})

	subHandle, err := existingMap.DumpAndSubscribe(nil, true)
	require.NoError(t, err)

	collect := 0
	done := make(chan struct{})
	go func(collect *int) {
		defer subHandle.Close()
		for range subHandle.C() {
			*collect++
		}
		close(done)
	}(&collect)

	key1 := &TestKey{Key: 103}
	value1 := &TestValue{Value: 203}
	err = existingMap.Update(key1, value1)
	require.NoError(t, err)
	err = existingMap.Update(key1, value1)
	require.NoError(t, err)
	err = existingMap.Delete(key1)
	require.NoError(t, err)

	subHandle.Close()
	<-done
	require.Equal(t, 3, collect)

	// cleanup
	err = existingMap.DeleteAll()
	existingMap.events = nil
	require.NoError(t, err)
}

func TestPrivilegedDump(t *testing.T) {
	testMap := setup(t)

	key1 := &TestKey{Key: 105}
	value1 := &TestValue{Value: 205}
	key2 := &TestKey{Key: 106}
	value2 := &TestValue{Value: 206}

	err := testMap.Update(key1, value1)
	require.NoError(t, err)
	err = testMap.Update(key2, value1)
	require.NoError(t, err)
	err = testMap.Update(key2, value2)
	require.NoError(t, err)

	dump1 := map[string][]string{}
	testMap.Dump(dump1)
	require.Equal(t, map[string][]string{
		"key=105": {"value=205"},
		"key=106": {"value=206"},
	}, dump1)

	dump2 := map[string][]string{}
	customCb := func(key MapKey, value MapValue) {
		dump2[key.String()] = append(dump2[key.String()], "custom-"+value.String())
	}
	testMap.DumpWithCallback(customCb)
	require.Equal(t, map[string][]string{
		"key=105": {"custom-value=205"},
		"key=106": {"custom-value=206"},
	}, dump2)

	dump3 := map[string][]string{}
	noSuchMap := NewMap("cilium_test_no_exist",
		ebpf.Hash, &TestKey{}, &TestValue{}, maxEntries, 0)
	err = noSuchMap.DumpIfExists(dump3)
	require.NoError(t, err)
	require.Empty(t, dump3)

	dump2 = map[string][]string{}
	err = noSuchMap.DumpWithCallbackIfExists(customCb)
	require.NoError(t, err)
	require.Empty(t, dump2)

	// Validate that if the key is zero, it shows up in dump output.
	keyZero := &TestKey{Key: 0}
	valueZero := &TestValue{Value: 0}
	err = testMap.Update(keyZero, valueZero)
	require.NoError(t, err)

	dump4 := map[string][]string{}
	customCb = func(key MapKey, value MapValue) {
		dump4[key.String()] = append(dump4[key.String()], "custom-"+value.String())
	}
	ds := NewDumpStats(testMap)
	err = testMap.DumpReliablyWithCallback(customCb, ds)
	require.NoError(t, err)
	require.Equal(t, map[string][]string{
		"key=0":   {"custom-value=0"},
		"key=105": {"custom-value=205"},
		"key=106": {"custom-value=206"},
	}, dump4)

	dump5 := map[string][]string{}
	err = testMap.Dump(dump5)
	require.NoError(t, err)
	require.Equal(t, map[string][]string{
		"key=0":   {"value=0"},
		"key=105": {"value=205"},
		"key=106": {"value=206"},
	}, dump5)
}

func TestPrivilegedDumpPerCPU(t *testing.T) {
	testMap := setupPerCPU(t)

	key1 := &TestKey{Key: 0}
	value1 := &TestValue{Value: 205}
	key2 := &TestKey{Key: 2}
	value2 := &TestValue{Value: 206}

	func() {
		testMap.lock.Lock()
		defer testMap.lock.Unlock()
		err := testMap.m.Update(key1, []any{value1}, ebpf.UpdateAny)
		require.NoError(t, err)
		err = testMap.m.Update(key2, []any{value1}, ebpf.UpdateAny)
		require.NoError(t, err)
		err = testMap.m.Update(key2, []any{value2}, ebpf.UpdateAny)
		require.NoError(t, err)
	}()

	dump := map[string][]uint32{}
	customCb := func(key MapKey, values any) {
		var value uint32
		for _, v := range *values.(*TestValues) {
			if value == 0 && v.Value != 0 {
				value = v.Value
			} else if value != 0 {
				require.Equal(t, uint32(0), v.Value)
			}
		}
		dump[key.String()] = append(dump[key.String()], value)
	}
	testMap.DumpPerCPUWithCallback(customCb)
	require.Equal(t, map[string][]uint32{
		"key=0": {205},
		"key=1": {0},
		"key=2": {206},
	}, dump)

	require.NoError(t, testMap.ClearAll())

	dump = map[string][]uint32{}
	testMap.DumpPerCPUWithCallback(customCb)
	require.Equal(t, map[string][]uint32{
		"key=0": {0},
		"key=1": {0},
		"key=2": {0},
	}, dump)
}

// TestPrivilegedDumpReliablyWithCallbackOverlapping attempts to test that DumpReliablyWithCallback
// will reliably iterate all keys that are known to be in a map, even if keys that are ahead
// of the current iteration can be deleted or updated concurrently.
// This test is not deterministic, it establishes a condition where we have keys that are known
// to be in the map and other keys which are volatile.  The test passes if the dump can reliably
// iterate all keys that are not volatile.
func TestPrivilegedDumpReliablyWithCallbackOverlapping(t *testing.T) {
	setup(t)

	iterations := 10000
	maxEntries := uint32(128)
	m := NewMap("cilium_dump_test2",
		ebpf.Hash,
		&TestKey{},
		&TestValue{},
		int(maxEntries),
		unix.BPF_F_NO_PREALLOC).WithCache()
	err := m.OpenOrCreate()
	require.NoError(t, err)
	defer func() {
		path, _ := m.Path()
		os.Remove(path)
	}()
	defer m.Close()

	// Prepopulate the map.
	for i := uint32(0); i < maxEntries; i++ {
		err := m.Update(&TestKey{Key: i}, &TestValue{Value: i + 200})
		require.NoError(t, err)
	}

	// used to block the update/delete goroutine so that both start at aprox the same time.
	start := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	wg := sync.WaitGroup{}
	wg.Add(1)
	// This goroutine will continuously delete and reinsert even keys.
	// Thus, when this is running in parallel with DumpReliablyWithCallback
	// it is unclear whether any even key will be iterated.
	go func() {
		defer wg.Done()
		<-start
		for {
			select {
			case <-ctx.Done():
				return
			default:
			}

			for i := uint32(0); i < maxEntries; i += 2 {
				m.Delete(&TestKey{Key: i})
				err := m.Update(&TestKey{Key: i}, &TestValue{Value: i + 200})
				require.NoError(t, err)
			}
		}
	}()

	// We expect that DumpReliablyWithCallback will iterate all odd key/value pairs
	// even if the even keys are being deleted and reinserted.
	expect := map[string]string{}
	for i := uint32(0); i < maxEntries; i++ {
		if i%2 != 0 {
			expect[fmt.Sprintf("key=%d", i)] = fmt.Sprintf("value=%d", i+200)
		}
	}
	close(start) // start testing.
	for range iterations {
		dump := map[string]string{}
		ds := NewDumpStats(m)
		err := m.DumpReliablyWithCallback(func(key MapKey, value MapValue) {
			k := key.(*TestKey).Key
			if k%2 != 0 {
				k := key.(*TestKey).Key
				ks := dump[fmt.Sprintf("key=%d", k)]
				if _, ok := dump[ks]; ok {
					t.FailNow()
				}
				dump[fmt.Sprintf("key=%d", key.(*TestKey).Key)] = fmt.Sprintf("value=%d", value.(*TestValue).Value)
			}
		}, ds)
		if err == nil {
			require.Equal(t, expect, dump)
		} else {
			require.Equal(t, ErrMaxLookup, err)
		}
	}
	cancel()
	wg.Wait()
}

// TestPrivilegedDumpReliablyWithCallback tests that DumpReliablyWithCallback by concurrently
// upserting/removing keys in range [0, 4) in the map and then continuously dumping
// the map.
// The test validates that all keys that are not being removed/added are contained in the dump.
func TestPrivilegedDumpReliablyWithCallback(t *testing.T) {
	setup(t)

	maxEntries := uint32(256)
	m := NewMap("cilium_dump_test",
		ebpf.Hash,
		&TestKey{},
		&TestValue{},
		int(maxEntries),
		unix.BPF_F_NO_PREALLOC).WithCache()
	err := m.OpenOrCreate()
	require.NoError(t, err)
	defer func() {
		path, _ := m.Path()
		os.Remove(path)
	}()
	defer m.Close()

	for i := uint32(4); i < maxEntries; i++ {
		err := m.Update(&TestKey{Key: i}, &TestValue{Value: i + 100})
		require.NoError(t, err) // we want to run the deferred calls
	}
	// start a goroutine that continuously updates the map
	started := make(chan struct{}, 1)
	done := make(chan struct{}, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		started <- struct{}{}
		for {
			for i := uint32(0); i < 4; i++ {
				if i < 3 {
					err := m.Update(&TestKey{Key: i}, &TestValue{Value: i + 100})
					// avoid assert to ensure we call wg.Done
					require.NoError(t, err)
				}
				if i > 0 {
					err := m.Delete(&TestKey{Key: i - 1})
					// avoid assert to ensure we call wg.Done
					require.NoError(t, err)
				}
			}
			select {
			case <-done:
				return
			default:
			}
		}
	}()
	<-started // wait until the routine has started to start the actual tests
	wg.Add(1)
	go func() {
		defer wg.Done()
		expect := map[string]string{}
		for i := uint32(4); i < maxEntries; i++ {
			expect[fmt.Sprintf("key=%d", i)] = fmt.Sprintf("custom-value=%d", i+100)
		}
		for i := range 100 {
			dump := map[string]string{}
			customCb := func(key MapKey, value MapValue) {
				k, err := strconv.ParseUint(strings.TrimPrefix(key.String(), "key="), 10, 32)
				require.NoError(t, err)
				if uint32(k) >= 4 {
					dump[key.String()] = "custom-" + value.String()
				}
			}
			ds := NewDumpStats(m)
			if i == 0 {
				// artificially trigger MaxLookupError as max lookup is based
				// on ds.MaxEntries
				ds.MaxEntries = 1
			}
			if err := m.DumpReliablyWithCallback(customCb, ds); err != nil {
				// avoid Assert to ensure the done signal is sent
				require.Equal(t, ErrMaxLookup, err)
			} else {
				// avoid Assert to ensure the done signal is sent
				require.Equal(t, expect, dump)
			}
		}
		done <- struct{}{}
	}()
	wg.Wait()
}

func TestPrivilegedDeleteAll(t *testing.T) {
	testMap := setup(t)

	key1 := &TestKey{Key: 105}
	value1 := &TestValue{Value: 205}
	key2 := &TestKey{Key: 106}
	value2 := &TestValue{Value: 206}

	err := testMap.Update(key1, value1)
	require.NoError(t, err)
	err = testMap.Update(key2, value1)
	require.NoError(t, err)
	err = testMap.Update(key2, value2)
	require.NoError(t, err)

	keyZero := &TestKey{Key: 0}
	valueZero := &TestValue{Value: 0}
	err = testMap.Update(keyZero, valueZero)
	require.NoError(t, err)

	dump1 := map[string][]string{}
	err = testMap.Dump(dump1)
	require.NoError(t, err)
	require.Equal(t, map[string][]string{
		"key=0":   {"value=0"},
		"key=105": {"value=205"},
		"key=106": {"value=206"},
	}, dump1)

	err = testMap.DeleteAll()
	require.NoError(t, err)

	dump2 := map[string][]string{}
	err = testMap.Dump(dump2)
	require.NoError(t, err)
}

func TestPrivilegedGetModel(t *testing.T) {
	testMap := setup(t)

	model := testMap.GetModel()
	require.NotNil(t, model)
}

func TestPrivilegedCheckAndUpgrade(t *testing.T) {
	setup(t)

	// CheckAndUpgrade removes map file if upgrade is needed
	// so we setup and use another map.
	upgradeMap := NewMap("cilium_test_upgrade",
		ebpf.Hash,
		&TestKey{},
		&TestValue{},
		maxEntries,
		unix.BPF_F_NO_PREALLOC).WithCache()
	err := upgradeMap.OpenOrCreate()
	require.NoError(t, err)
	defer func() {
		_ = upgradeMap.Unpin()
		upgradeMap.Close()
	}()

	// Exactly the same MapInfo so it won't be upgraded.
	upgrade := upgradeMap.CheckAndUpgrade(upgradeMap)
	require.False(t, upgrade)

	// preallocMap unsets unix.BPF_F_NO_PREALLOC so upgrade is needed.
	EnableMapPreAllocation()
	preallocMap := NewMap("cilium_test_upgrade",
		ebpf.Hash,
		&TestKey{},
		&TestValue{},
		maxEntries,
		0).WithCache()
	upgrade = upgradeMap.CheckAndUpgrade(preallocMap)
	require.True(t, upgrade)
	DisableMapPreAllocation()
}

func TestPrivilegedUnpin(t *testing.T) {
	setup(t)

	var exist bool
	unpinMap := NewMap("cilium_test_unpin",
		ebpf.Hash,
		&TestKey{},
		&TestValue{},
		maxEntries,
		unix.BPF_F_NO_PREALLOC).WithCache()
	err := unpinMap.OpenOrCreate()
	require.NoError(t, err)
	exist, err = unpinMap.exist()
	require.NoError(t, err)
	require.True(t, exist)

	err = unpinMap.Unpin()
	require.NoError(t, err)
	exist, err = unpinMap.exist()
	require.NoError(t, err)
	require.False(t, exist)

	err = unpinMap.UnpinIfExists()
	require.NoError(t, err)
	exist, err = unpinMap.exist()
	require.NoError(t, err)
	require.False(t, exist)

	err = unpinMap.Unpin()
	require.NoError(t, err)
	err = unpinMap.OpenOrCreate()
	require.NoError(t, err)
	err = unpinMap.Unpin()
	require.NoError(t, err)
	exist, err = unpinMap.exist()
	require.NoError(t, err)
	require.False(t, exist)
}

func TestPrivilegedCreateUnpinned(t *testing.T) {
	setup(t)

	m := NewMap("cilium_test_create_unpinned",
		ebpf.Hash,
		&TestKey{},
		&TestValue{},
		maxEntries,
		unix.BPF_F_NO_PREALLOC).WithCache()
	err := m.CreateUnpinned()
	require.NoError(t, err)
	exist, err := m.exist()
	require.NoError(t, err)
	require.False(t, exist)

	k := &TestKey{Key: 105}
	v := &TestValue{Value: 205}
	err = m.Update(k, v)
	require.NoError(t, err)

	got, err := m.Lookup(k)
	require.NoError(t, err)
	require.EqualValues(t, v, got)
}

func BenchmarkMapLookup(b *testing.B) {
	b.ReportAllocs()

	m := NewMap("",
		ebpf.Hash,
		&TestKey{},
		&TestValue{},
		1,
		unix.BPF_F_NO_PREALLOC)

	if err := m.CreateUnpinned(); err != nil {
		b.Fatal(err)
	}

	k := TestKey{Key: 0}
	if err := m.Update(&k, &TestValue{Value: 1}); err != nil {
		b.Fatal(err)
	}

	for b.Loop() {
		if _, err := m.Lookup(&k); err != nil {
			b.Fatal(err)
		}
	}
}

func TestPrivilegedErrorResolver(t *testing.T) {
	testutils.PrivilegedTest(t)
	logger := hivetest.Logger(t)
	CheckOrMountFS(logger, "")
	require.NoError(t, rlimit.RemoveMemlock())

	var (
		key1, key2 = TestKey{Key: 10}, TestKey{Key: 20}
		val1, val2 = TestValue{1}, TestValue{2}
	)

	tests := []struct {
		name        string
		remove      func(t *testing.T, m *Map)
		expectedKey TestKey
		expectedVal TestValue
	}{
		{
			name:        "remove inserted element",
			remove:      func(t *testing.T, m *Map) { require.NoError(t, m.Delete(&key1), "Failed to remove element from map") },
			expectedKey: key2,
			expectedVal: val2,
		},
		{
			name:        "remove failing element",
			remove:      func(t *testing.T, m *Map) { require.Error(t, m.Delete(&key2), "Removal from map should have failed") },
			expectedKey: key1,
			expectedVal: val1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewMap("cilium_error_resolver_test",
				ebpf.Hash,
				&TestKey{},
				&TestValue{},
				1, // Only one entry, so that the second insertion will fail
				unix.BPF_F_NO_PREALLOC,
			).WithCache()

			t.Cleanup(func() {
				// Let's make sure that there's no interference between tests
				mapControllers.RemoveControllerAndWait(m.controllerName())
			})

			require.NoError(t, m.CreateUnpinned(), "Failed to create map")
			require.NoError(t, m.Update(&key1, &val1), "Failed to insert element in map")

			// Let's attempt to insert a second element in the map, which will fail because the map can only hold one.
			require.Error(t, m.Update(&key2, &val2), "Map insertion should have failed")

			// Let's now remove one of the two elements (the actual assertion depends on which element is to be removed)
			tt.remove(t, m)

			// Assert that the other element is eventually present and correct
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				value, err := m.Lookup(&tt.expectedKey)
				assert.NoError(c, err)
				if assert.NotNil(c, value) {
					assert.Equal(c, tt.expectedVal.Value, value.(*TestValue).Value)
				}
			}, timeout, tick)

			// Check that the error resolver controller eventually succeeds
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				models := mapControllers.GetStatusModel()
				for _, model := range models {
					if model.Name == m.controllerName() {
						assert.NotZero(c, model.Status.SuccessCount)
						assert.Greater(c, model.Status.LastSuccessTimestamp, model.Status.LastFailureTimestamp)
						return
					}
				}

				assert.Fail(c, "Expected controller status not found")
			}, timeout, tick)
		})
	}
}

func TestBatchIteratorTypes(t *testing.T) {
	m := NewMap("cilium_test",
		ebpf.Array,
		&TestKey{},
		&TestValue{}, 1, 0)
	iter := NewBatchIterator[TestKey, TestValue](m)
	iter.IterateAll(context.TODO())
	assert.Error(t, iter.Err())
	assert.NotNil(t, iter)
}

func TestPrivilegedBatchIterator(t *testing.T) {
	testutils.PrivilegedTest(t)

	runTest := func(mapType ebpf.MapType, size, mapSize int, t *testing.T, opts ...BatchIteratorOpt[TestLPMKey, TestValue, *TestLPMKey, *TestValue]) {
		makeKey := func(i int) MapKey {
			// Note: We use a lpm key type as it is compatible with lpmtrie map tests
			// and works fine for other tests.
			return &TestLPMKey{
				PrefixLen: 32,
				Key:       uint32(i),
			}
		}
		m := NewMap("cilium_test",
			mapType,
			makeKey(0),
			&TestValue{},
			mapSize,
			0,
		)
		require.NoError(t, m.OpenOrCreate())
		defer assert.NoError(t, m.UnpinIfExists())
		for i := range size {
			mapKey := makeKey(i)
			mapValue := &TestValue{Value: uint32(i)}
			err := m.Update(mapKey, mapValue)
			assert.NoError(t, err)
		}
		ks := sets.New[int]()
		vs := sets.New[int]()

		iter := NewBatchIterator[TestLPMKey, TestValue](m)
		count := 0
		for k, v := range iter.IterateAll(context.TODO(), opts...) {
			count++
			ks.Insert(int(k.Key))
			vs.Insert(int(v.Value))
		}
		require.NoError(t, iter.Err())
		assert.Equal(t, size, count)

		for i := range int(size) {
			require.Contains(t, ks, i, "expect iterate to return key="+strconv.Itoa(i))
			require.Contains(t, vs, i, "expect iterate to return val="+strconv.Itoa(i))
		}
		assert.Len(t, ks, int(size))
		assert.Len(t, vs, int(size))
		assert.Equal(t, size, count)

		count, err := m.BatchCount()
		require.NoError(t, err, "BatchCount")
		assert.Equal(t, size, count)
	}

	for _, test := range []struct {
		mapSize int
		size    int
		opts    []BatchIteratorOpt[TestLPMKey, TestValue, *TestLPMKey, *TestValue]
		// LRU hash maps aren't totally safe to test like this, even if you're
		// within the max map size number of elements, in practice the kernel
		// will occasionally do a LRU eviction causing failures.
		// Setting the max size appears to make this safe enough (test up to a
		// test million runs) that we can run a subset of tests on LRU.
		unsafeLRU bool
	}{
		{10, 10, nil, true},
		{1024, 1024, nil, true},
		{1048576, 1024, nil, false}, // Max map size much larger means no chance of LRU eviction.
		// Setup iteration that starts with batch size of 1, this is bound to fail at some point
		// so this will test if the chunk size growth retry loop works correctly.
		{
			size:    1 << 12,
			mapSize: 1 << 13,
			opts: []BatchIteratorOpt[TestLPMKey, TestValue, *TestLPMKey, *TestValue]{
				WithMaxRetries[TestLPMKey, TestValue](13), WithStartingChunkSize[TestLPMKey, TestValue](1)},
			unsafeLRU: true,
		},
		{
			size:    1,
			mapSize: 1 << 12,
		},
		{
			size:    1 << 12,
			mapSize: 1 << 12,
			opts: []BatchIteratorOpt[TestLPMKey, TestValue, *TestLPMKey, *TestValue]{
				WithMaxRetries[TestLPMKey, TestValue](1), WithStartingChunkSize[TestLPMKey, TestValue](1 << 13)},
			unsafeLRU: true,
		},
	} {
		t.Run(fmt.Sprintf("hash size=%d mapSize=%d", test.size, test.mapSize), func(t *testing.T) {
			runTest(ebpf.Hash, test.size, test.mapSize, t, test.opts...)
		})
		t.Run(fmt.Sprintf("lpmtrie size=%d mapSize=%d", test.size, test.mapSize), func(t *testing.T) {
			runTest(ebpf.LPMTrie, test.size, test.mapSize, t, test.opts...)
		})
		if !test.unsafeLRU {
			t.Run(fmt.Sprintf("hashlru size=%d mapSize=%d", test.size, test.mapSize), func(t *testing.T) {
				runTest(ebpf.LRUHash, test.size, test.mapSize, t, test.opts...)
			})
		}
	}
}
