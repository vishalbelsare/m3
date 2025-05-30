// Copyright (c) 2016 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package etcd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"sync"

	"github.com/golang/protobuf/proto"
	"github.com/uber-go/tally"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"
	"golang.org/x/net/context"

	"github.com/m3db/m3/src/cluster/etcd/watchmanager"
	"github.com/m3db/m3/src/cluster/kv"
	xerrors "github.com/m3db/m3/src/x/errors"
	"github.com/m3db/m3/src/x/retry"
)

const etcdVersionZero = 0

var (
	noopCancel               func()
	emptyCmp                 clientv3.Cmp
	emptyOp                  clientv3.Op
	errInvalidHistoryVersion = errors.New("invalid version range")
	errNilPutResponse        = errors.New("nil put response from etcd")
)

// NewStore creates a kv store based on etcd and watches single keys.
func NewStore(etcdKV *clientv3.Client, opts Options) (kv.TxnStore, error) {
	store := newStore[kv.Value, kv.ValueWatch](etcdKV, opts)
	clientWatchOpts := newClientWatchOptions(opts)
	wOpts := watchmanager.NewOptions().
		SetClient(etcdKV).
		SetUpdateFn(store.update).
		SetTickAndStopFn(store.tickAndStop).
		SetWatchOptions(clientWatchOpts).
		SetWatchChanCheckInterval(opts.WatchChanCheckInterval()).
		SetWatchChanInitTimeout(opts.WatchChanInitTimeout()).
		SetWatchChanResetInterval(opts.WatchChanResetInterval()).
		SetInstrumentsOptions(opts.InstrumentsOptions())

	wm, err := watchmanager.NewWatchManager(wOpts)
	if err != nil {
		return nil, err
	}

	store.wm = wm
	return store, nil
}

// NewPrefixStore creates a kv store based on etcd and watches all keys with a given prefix.
func NewPrefixStore(etcdKV *clientv3.Client, opts Options) (kv.PrefixStore, error) {
	store := newStore[map[string]interface{}, kv.PrefixWatch](
		etcdKV,
		opts,
	)

	clientWatchOpts := newClientWatchOptions(opts)
	clientWatchOpts = append(clientWatchOpts, []clientv3.OpOption{clientv3.WithPrefix()}...)
	wOpts := watchmanager.NewOptions().
		SetClient(etcdKV).
		SetUpdateFn(store.updateForPrefix).
		SetTickAndStopFn(store.tickAndStop).
		SetWatchOptions(clientWatchOpts).
		SetWatchChanCheckInterval(opts.WatchChanCheckInterval()).
		SetWatchChanInitTimeout(opts.WatchChanInitTimeout()).
		SetWatchChanResetInterval(opts.WatchChanResetInterval()).
		SetInstrumentsOptions(opts.InstrumentsOptions())

	wm, err := watchmanager.NewWatchManager(wOpts)
	if err != nil {
		return nil, err
	}

	store.wm = wm
	return store, nil
}

func newStore[ValueType any, ValueWatchType any](
	etcdKV *clientv3.Client,
	opts Options,
) *client[ValueType, ValueWatchType] {
	scope := opts.InstrumentsOptions().MetricsScope()

	store := &client[ValueType, ValueWatchType]{
		opts:           opts,
		kv:             etcdKV,
		watchables:     map[string]kv.Watchable[ValueType, ValueWatchType]{},
		retrier:        retry.NewRetrier(opts.RetryOptions()),
		logger:         opts.InstrumentsOptions().Logger(),
		cacheFile:      opts.CacheFileFn()(opts.Prefix()),
		cache:          newCache(),
		cacheUpdatedCh: make(chan struct{}, 1),
		m: clientMetrics{
			etcdGetError:   scope.Counter("etcd-get-error"),
			etcdPutError:   scope.Counter("etcd-put-error"),
			etcdTnxError:   scope.Counter("etcd-tnx-error"),
			diskWriteError: scope.Counter("disk-write-error"),
			diskReadError:  scope.Counter("disk-read-error"),
		},
	}

	if store.cacheFile != "" {
		if err := store.initCache(opts.NewDirectoryMode()); err != nil {
			store.logger.Warn("could not load cache from file", zap.String("file", store.cacheFile), zap.Error(err))
		} else {
			store.logger.Info("successfully loaded cache from file", zap.String("file", store.cacheFile))
		}

		go func() {
			for range store.cacheUpdatedCh {
				if err := store.writeCacheToFile(); err != nil {
					store.logger.Warn("failed to write cache to file", zap.Error(err))
				}
			}
		}()
	}
	return store
}

func newClientWatchOptions(opts Options) []clientv3.OpOption {
	clientWatchOpts := []clientv3.OpOption{
		// periodically (appx every 10 mins) checks for the latest data
		// with or without any update notification
		clientv3.WithProgressNotify(),
		// receive initial notification once the watch channel is created
		clientv3.WithCreatedNotify(),
	}

	if rev := opts.WatchWithRevision(); rev > 0 {
		clientWatchOpts = append(clientWatchOpts, clientv3.WithRev(rev))
	}

	return clientWatchOpts
}

//nolint:structcheck
type client[ValueType any, ValueWatchType any] struct {
	sync.RWMutex

	opts           Options
	kv             *clientv3.Client
	watchables     map[string]kv.Watchable[ValueType, ValueWatchType]
	retrier        retry.Retrier
	logger         *zap.Logger
	m              clientMetrics
	cache          *valueCache
	cacheFile      string
	cacheUpdatedCh chan struct{}

	wm watchmanager.WatchManager
}

type clientMetrics struct {
	etcdGetError   tally.Counter
	etcdPutError   tally.Counter
	etcdTnxError   tally.Counter
	diskWriteError tally.Counter
	diskReadError  tally.Counter
}

// Get returns the latest value from etcd store and only fall back to
// in-memory cache if the remote store is unavailable
func (c *client[ValueType, ValueWatchType]) Get(key string) (kv.Value, error) {
	return c.get(c.opts.ApplyPrefix(key))
}

func (c *client[ValueType, ValueWatchType]) get(key string) (kv.Value, error) {
	ctx, cancel := c.context()
	defer cancel()

	var opts []clientv3.OpOption
	if c.opts.EnableFastGets() {
		opts = append(opts, clientv3.WithSerializable())
	}
	r, err := c.kv.Get(ctx, key, opts...)
	if err != nil {
		c.m.etcdGetError.Inc(1)
		cachedV, ok := c.getCache(key)
		if ok {
			return cachedV, nil
		}
		return nil, err
	}

	if r.Count == 0 {
		c.deleteCache(key) // delete cache entry if it exists
		return nil, kv.ErrNotFound
	}

	v := newValue(r.Kvs[0].Value, r.Kvs[0].Version, r.Kvs[0].ModRevision)

	c.mergeCache(key, v)

	return v, nil
}

func (c *client[ValueType, ValueWatchType]) GetForPrefix(prefix string) (map[string]interface{}, error) {
	return c.getForPrefix(c.opts.ApplyPrefix(prefix))
}

func (c *client[ValueType, ValueWatchType]) getForPrefix(prefix string) (map[string]interface{}, error) {
	ctx, cancel := c.context()
	defer cancel()

	var opts []clientv3.OpOption
	if c.opts.EnableFastGets() {
		opts = append(opts, clientv3.WithSerializable())
	}
	opts = append(opts, clientv3.WithPrefix())
	r, err := c.kv.Get(ctx, prefix, opts...)
	if err != nil {
		c.m.etcdGetError.Inc(1)
		return nil, err
	}

	if r.Count == 0 {
		return nil, kv.ErrNotFound
	}

	values := make(map[string]interface{})
	for _, kv := range r.Kvs {
		values[string(kv.Key)] = newValue(kv.Value, kv.Version, kv.ModRevision)
	}

	return values, nil
}

func (c *client[ValueType, ValueWatchType]) History(key string, from, to int) ([]kv.Value, error) {
	if from > to || from < 0 || to < 0 {
		return nil, errInvalidHistoryVersion
	}

	if from == to {
		return nil, nil
	}

	newKey := c.opts.ApplyPrefix(key)

	ctx, cancel := c.context()
	defer cancel()

	r, err := c.kv.Get(ctx, newKey)
	if err != nil {
		return nil, err
	}

	if r.Count == 0 {
		return nil, kv.ErrNotFound
	}

	numValue := to - from

	latestKV := r.Kvs[0]
	version := int(latestKV.Version)
	modRev := latestKV.ModRevision

	if version < from {
		// no value available in the requested version range
		return nil, nil
	}

	if version-from+1 < numValue {
		// get the correct size of the result slice
		numValue = version - from + 1
	}

	res := make([]kv.Value, numValue)

	if version < to {
		// put it in the last element of the result
		res[version-from] = newValue(latestKV.Value, latestKV.Version, modRev)
	}

	for version > from {
		ctx, cancel := c.context()
		defer cancel()

		r, err = c.kv.Get(ctx, newKey, clientv3.WithRev(modRev-1))
		if err != nil {
			return nil, err
		}

		if r.Count == 0 {
			// unexpected
			return nil, fmt.Errorf("could not find version %d for key %s", version-1, key)
		}

		v := r.Kvs[0]
		modRev = v.ModRevision
		version = int(v.Version)
		if version < to {
			res[version-from] = newValue(v.Value, v.Version, v.ModRevision)
		}
	}

	return res, nil
}

func (c *client[ValueType, ValueWatchType]) processCondition(condition kv.Condition) (clientv3.Cmp, error) {
	var cmp clientv3.Cmp
	switch condition.TargetType() {
	case kv.TargetVersion:
		cmp = clientv3.Version(c.opts.ApplyPrefix(condition.Key()))
	default:
		return emptyCmp, kv.ErrUnknownTargetType
	}

	var compareStr string
	switch condition.CompareType() {
	case kv.CompareEqual:
		compareStr = condition.CompareType().String()
	default:
		return emptyCmp, kv.ErrUnknownCompareType
	}

	return clientv3.Compare(cmp, compareStr, condition.Value()), nil
}

func (c *client[ValueType, ValueWatchType]) processOp(op kv.Op) (clientv3.Op, error) {
	switch op.Type() {
	case kv.OpSet:
		opSet := op.(kv.SetOp)

		value, err := proto.Marshal(opSet.Value)
		if err != nil {
			return emptyOp, err
		}

		return clientv3.OpPut(
			c.opts.ApplyPrefix(opSet.Key()),
			string(value),
			clientv3.WithPrevKV(),
		), nil
	default:
		return emptyOp, kv.ErrUnknownOpType
	}
}

func (c *client[ValueType, ValueWatchType]) Commit(conditions []kv.Condition, ops []kv.Op) (kv.Response, error) {
	ctx, cancel := c.context()
	defer cancel()

	txn := c.kv.Txn(ctx)

	cmps := make([]clientv3.Cmp, len(conditions))
	for i, condition := range conditions {
		cmp, err := c.processCondition(condition)
		if err != nil {
			return nil, err
		}

		cmps[i] = cmp
	}

	txn = txn.If(cmps...)

	etcdOps := make([]clientv3.Op, len(ops))
	opResponses := make([]kv.OpResponse, len(ops))
	for i, op := range ops {
		etcdOp, err := c.processOp(op)
		if err != nil {
			return nil, err
		}

		etcdOps[i] = etcdOp
		opResponses[i] = kv.NewOpResponse(op)
	}

	txn = txn.Then(etcdOps...)

	r, err := txn.Commit()
	if err != nil {
		c.m.etcdTnxError.Inc(1)
		return nil, err
	}
	if !r.Succeeded {
		return nil, kv.ErrConditionCheckFailed
	}

	for i := range r.Responses {
		opr := opResponses[i]
		switch opr.Type() {
		case kv.OpSet:
			res := r.Responses[i].GetResponsePut()
			if res == nil {
				return nil, errNilPutResponse
			}

			if res.PrevKv != nil {
				opr = opr.SetValue(int(res.PrevKv.Version + 1))
			} else {
				opr = opr.SetValue(etcdVersionZero + 1)
			}
		}

		opResponses[i] = opr
	}

	return kv.NewResponse().SetResponses(opResponses), nil
}

func (c *client[ValueType, ValueWatchType]) Watch(key string) (ValueWatchType, error) {
	newKey := c.opts.ApplyPrefix(key)
	c.Lock()
	w, ok := c.watchables[newKey]
	if !ok {
		watchable := kv.NewValueWatchable()
		w = watchable.(kv.Watchable[ValueType, ValueWatchType])
		c.watchables[newKey] = w

		go c.wm.Watch(newKey)

	}
	c.Unlock()
	_, watch, err := w.Watch()
	return watch, err
}

func (c *client[ValueType, ValueWatchType]) WatchForPrefix(prefix string) (ValueWatchType, error) {
	newKey := c.opts.ApplyPrefix(prefix)
	c.Lock()
	w, ok := c.watchables[newKey]
	if !ok {
		watchable := kv.NewPrefixWatchable()
		w = watchable.(kv.Watchable[ValueType, ValueWatchType])
		c.watchables[newKey] = w
		go c.wm.Watch(newKey)
	}

	c.Unlock()

	_, watch, err := w.Watch()
	return watch, err
}

func (c *client[ValueType, ValueWatchType]) getFromKVStore(key string) (kv.Value, error) {
	var (
		nv  kv.Value
		err error
	)
	if execErr := c.retrier.Attempt(func() error {
		nv, err = c.get(key)
		if err == kv.ErrNotFound {
			// do not retry on ErrNotFound
			return retry.NonRetryableError(err)
		}
		return err
	}); execErr != nil && xerrors.GetInnerNonRetryableError(execErr) != kv.ErrNotFound {
		return nil, execErr
	}

	return nv, nil
}

func (c *client[ValueType, ValueWatchType]) getFromEtcdEvents(key string, events []*clientv3.Event) kv.Value {
	lastEvent := events[len(events)-1]
	if lastEvent.Type == clientv3.EventTypeDelete {
		c.deleteCache(key)
		return nil
	}

	nv := newValue(lastEvent.Kv.Value, lastEvent.Kv.Version, lastEvent.Kv.ModRevision)
	c.mergeCache(key, nv)
	return nv
}

func (c *client[ValueType, ValueWatchType]) getFromEtcdEventsForPrefix(
	events []*clientv3.Event,
) (map[string]interface{}, []string) {
	toDelete := []string{}

	values := make(map[string]interface{})
	for _, e := range events {
		if e.Type == clientv3.EventTypeDelete {
			toDelete = append(toDelete, string(e.Kv.Key))
			continue
		}

		values[string(e.Kv.Key)] = newValue(e.Kv.Value, e.Kv.Version, e.Kv.ModRevision)
	}

	return values, toDelete
}

func (c *client[ValueType, ValueWatchType]) getFromKVStoreForPrefix(prefix string) (map[string]interface{}, error) {
	var (
		nv  map[string]interface{}
		err error
	)
	if execErr := c.retrier.Attempt(func() error {
		nv, err = c.getForPrefix(prefix)
		if errors.Is(err, kv.ErrNotFound) {
			// do not retry on ErrNotFound
			return retry.NonRetryableError(err)
		}
		return err
	}); execErr != nil && errors.Is(xerrors.GetInnerNonRetryableError(execErr), kv.ErrNotFound) {
		return nil, execErr
	}

	return nv, nil
}

func (c *client[ValueType, ValueWatchType]) update(key string, events []*clientv3.Event) error {
	var nv kv.Value
	if len(events) == 0 {
		var err error
		if nv, err = c.getFromKVStore(key); err != nil {
			// This is triggered by initializing a new watch and no value available for the key.
			return nil
		}
	} else {
		nv = c.getFromEtcdEvents(key, events)
	}

	c.RLock()
	w, ok := c.watchables[key]
	c.RUnlock()
	if !ok {
		return fmt.Errorf("unexpected: no watchable found for key: %s", key)
	}

	watchable, ok := any(w).(kv.ValueWatchable)
	if !ok {
		return fmt.Errorf("unexpected: value watchable not found for key: %s", key)
	}

	curValue := watchable.Get()

	// Both current and new are nil.
	if curValue == nil && nv == nil {
		return nil
	}

	if nv == nil {
		// At deletion, just update the watch to nil.
		return watchable.Update(nil)
	}

	if curValue == nil || nv.IsNewer(curValue) {
		return watchable.Update(nv)
	}

	return nil
}

func (c *client[ValueType, ValueWatchType]) updateForPrefix(prefix string, events []*clientv3.Event) error {
	var (
		values   map[string]interface{}
		toDelete []string
	)
	if len(events) == 0 {
		var err error
		if values, err = c.getFromKVStoreForPrefix(prefix); err != nil {
			// This is triggered by initializing a new watch and no value available for the key.
			return nil
		}
	} else {
		values, toDelete = c.getFromEtcdEventsForPrefix(events)
	}

	c.RLock()
	w, ok := c.watchables[prefix]
	c.RUnlock()
	if !ok {
		return fmt.Errorf("unexpected: no watchable found for key: %s", prefix)
	}

	watchable, ok := any(w).(kv.PrefixWatchable)
	if !ok {
		return fmt.Errorf("unexpected: prefix watchable not found for path: %s", prefix)
	}

	curValues := watchable.Get()

	// Both current and new are empty.
	if len(curValues) == 0 && len(values) == 0 {
		return nil
	}

	// combine the current and new values
	updatedValues := make(map[string]interface{})
	for k, v := range curValues {
		updatedValues[k] = v
	}
	for k, v := range values {
		updatedValues[k] = v
	}

	// delete the keys that are no longer present
	for _, k := range toDelete {
		delete(updatedValues, k)
	}

	// number of updated values is different from the current values, so we need to update the watch
	if len(updatedValues) != len(curValues) {
		return watchable.Update(updatedValues)
	}

	// check if any of the values were updated
	for k, v := range values {
		existing, ok := curValues[k]
		if !ok {
			return watchable.Update(updatedValues)
		}

		newValue := v.(kv.Value)
		existingValue := existing.(kv.Value)
		if newValue.IsNewer(existingValue) {
			return watchable.Update(updatedValues)
		}
	}

	return nil
}

func (c *client[ValueType, ValueWatchType]) tickAndStop(key string) bool {
	// fast path
	c.RLock()
	watchable, ok := c.watchables[key]
	c.RUnlock()
	if !ok {
		c.logger.Warn("unexpected: key is already cleaned up", zap.String("key", key))
		return true
	}

	if watchable.NumWatches() != 0 {
		return false
	}

	// slow path
	c.Lock()
	defer c.Unlock()
	watchable, ok = c.watchables[key]
	if !ok {
		// not expect this to happen
		c.logger.Warn("unexpected: key is already cleaned up", zap.String("key", key))
		return true
	}

	if watchable.NumWatches() != 0 {
		// a new watch has subscribed to the watchable, do not clean up
		return false
	}

	watchable.Close()
	delete(c.watchables, key)
	return true
}

func (c *client[ValueType, ValueWatchType]) Set(key string, v proto.Message) (int, error) {
	ctx, cancel := c.context()
	defer cancel()

	value, err := proto.Marshal(v)
	if err != nil {
		return 0, err
	}

	r, err := c.kv.Put(ctx, c.opts.ApplyPrefix(key), string(value), clientv3.WithPrevKV())
	if err != nil {
		c.m.etcdPutError.Inc(1)
		return 0, err
	}

	// if there is no prev kv, means this is the first version of the key
	if r.PrevKv == nil {
		return etcdVersionZero + 1, nil
	}

	return int(r.PrevKv.Version + 1), nil
}

func (c *client[ValueType, ValueWatchType]) SetIfNotExists(key string, v proto.Message) (int, error) {
	version, err := c.CheckAndSet(key, etcdVersionZero, v)
	if err == kv.ErrVersionMismatch {
		err = kv.ErrAlreadyExists
	}
	return version, err
}

func (c *client[ValueType, ValueWatchType]) CheckAndSet(key string, version int, v proto.Message) (int, error) {
	ctx, cancel := c.context()
	defer cancel()

	value, err := proto.Marshal(v)
	if err != nil {
		return 0, err
	}

	key = c.opts.ApplyPrefix(key)
	r, err := c.kv.Txn(ctx).
		If(clientv3.Compare(clientv3.Version(key), kv.CompareEqual.String(), version)).
		Then(clientv3.OpPut(key, string(value))).
		Commit()
	if err != nil {
		c.m.etcdTnxError.Inc(1)
		return 0, err
	}
	if !r.Succeeded {
		return 0, kv.ErrVersionMismatch
	}

	return version + 1, nil
}

func (c *client[ValueType, ValueWatchType]) Delete(key string) (kv.Value, error) {
	ctx, cancel := c.context()
	defer cancel()

	key = c.opts.ApplyPrefix(key)

	r, err := c.kv.Delete(ctx, key, clientv3.WithPrevKV())
	if err != nil {
		return nil, err
	}

	if r.Deleted == 0 {
		return nil, kv.ErrNotFound
	}

	prevKV := newValue(r.PrevKvs[0].Value, r.PrevKvs[0].Version, r.PrevKvs[0].ModRevision)

	c.deleteCache(key)

	return prevKV, nil
}

func (c *client[ValueType, ValueWatchType]) deleteCache(key string) {
	c.cache.Lock()
	defer c.cache.Unlock()

	// only do a delete if we actually need to
	_, found := c.cache.Values[key]
	if !found {
		return
	}

	delete(c.cache.Values, key)
	c.notifyCacheUpdate()
}

func (c *client[ValueType, ValueWatchType]) getCache(key string) (kv.Value, bool) {
	c.cache.RLock()
	v, ok := c.cache.Values[key]
	c.cache.RUnlock()

	return v, ok
}

func (c *client[ValueType, ValueWatchType]) mergeCache(key string, v *value) {
	c.cache.Lock()

	cur, ok := c.cache.Values[key]
	if !ok || v.IsNewer(cur) {
		c.cache.Values[key] = v
		c.notifyCacheUpdate()
	}

	c.cache.Unlock()
}

func (c *client[ValueType, ValueWatchType]) notifyCacheUpdate() {
	// notify that cached data is updated
	select {
	case c.cacheUpdatedCh <- struct{}{}:
	default:
	}
}

func (c *client[ValueType, ValueWatchType]) writeCacheToFile() error {
	file, err := os.Create(c.cacheFile)
	if err != nil {
		c.m.diskWriteError.Inc(1)
		c.logger.Warn("error creating cache file", zap.String("file", c.cacheFile), zap.Error(err))
		return fmt.Errorf("invalid cache file: %s", c.cacheFile)
	}

	encoder := json.NewEncoder(file)
	c.cache.RLock()
	err = encoder.Encode(c.cache)
	c.cache.RUnlock()

	if err != nil {
		c.m.diskWriteError.Inc(1)
		c.logger.Warn("error encoding values", zap.Error(err))
		return err
	}

	if err = file.Close(); err != nil {
		c.m.diskWriteError.Inc(1)
		c.logger.Warn("error closing cache file", zap.String("file", c.cacheFile), zap.Error(err))
	}

	return nil
}

func (c *client[ValueType, ValueWatchType]) createCacheDir(fm os.FileMode) error {
	path := path.Dir(c.opts.CacheFileFn()(c.opts.Prefix()))
	if err := os.MkdirAll(path, fm); err != nil {
		c.m.diskWriteError.Inc(1)
		c.logger.Warn("error creating cache directory",
			zap.String("path", path),
			zap.Error(err),
		)
		return err
	}

	c.logger.Info("successfully created new cache dir",
		zap.String("path", path),
		zap.Int("mode", int(fm)),
	)

	return nil
}

func (c *client[ValueType, ValueWatchType]) initCache(fm os.FileMode) error {
	if err := c.createCacheDir(fm); err != nil {
		c.m.diskWriteError.Inc(1)
		return fmt.Errorf("error creating cache directory: %s", err)
	}
	file, err := os.Open(c.cacheFile)
	if err != nil {
		c.m.diskReadError.Inc(1)
		return fmt.Errorf("error opening cache file %s: %v", c.cacheFile, err)
	}

	// Read bootstrap file
	decoder := json.NewDecoder(file)

	if err := decoder.Decode(c.cache); err != nil {
		c.m.diskReadError.Inc(1)
		return fmt.Errorf("error reading cache file %s: %v", c.cacheFile, err)
	}

	return nil
}

func (c *client[ValueType, ValueWatchType]) context() (context.Context, context.CancelFunc) {
	ctx := context.Background()
	cancel := noopCancel
	if c.opts.RequestTimeout() > 0 {
		ctx, cancel = context.WithTimeout(ctx, c.opts.RequestTimeout())
	}

	return ctx, cancel
}

type valueCache struct {
	sync.RWMutex

	Values map[string]*value `json:"values"`
}

func newCache() *valueCache {
	return &valueCache{Values: make(map[string]*value)}
}

type value struct {
	Val []byte `json:"value"`
	Ver int64  `json:"version"`
	Rev int64  `json:"revision"`
}

func newValue(val []byte, ver, rev int64) *value {
	return &value{
		Val: val,
		Ver: ver,
		Rev: rev,
	}
}

func (c *value) IsNewer(other kv.Value) bool {
	othervalue, ok := other.(*value)
	if ok {
		return c.Rev > othervalue.Rev
	}

	return c.Version() > other.Version()
}

func (c *value) Unmarshal(v proto.Message) error {
	err := proto.Unmarshal(c.Val, v)

	return err
}

func (c *value) Version() int {
	return int(c.Ver)
}
