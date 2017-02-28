package collection

import (
	"context"
	"fmt"
	"path"
	"reflect"
	"strconv"
	"strings"

	"github.com/pachyderm/pachyderm/src/server/pkg/watch"

	etcd "github.com/coreos/etcd/clientv3"
	"github.com/gogo/protobuf/proto"
)

type collection struct {
	etcdClient *etcd.Client
	prefix     string
	indexes    []Index
}

func NewCollection(etcdClient *etcd.Client, prefix string, indexes []Index) Collection {
	// We want to ensure that the prefix always ends with a trailing
	// slash.  Otherwise, when you list the items under a collection
	// such as `foo`, you might end up listing items under `foobar`
	// as well.
	if len(prefix) > 0 && prefix[len(prefix)-1] != '/' {
		prefix = prefix + "/"
	}

	return &collection{
		prefix:     prefix,
		etcdClient: etcdClient,
		indexes:    indexes,
	}
}

func (c *collection) ReadWrite(stm STM) ReadWriteCollection {
	return &readWriteCollection{
		collection: c,
		stm:        stm,
	}
}

func (c *collection) ReadWriteInt(stm STM) ReadWriteIntCollection {
	return &readWriteIntCollection{
		collection: c,
		stm:        stm,
	}
}

func (c *collection) ReadOnly(ctx context.Context) ReadonlyCollection {
	return &readonlyCollection{
		collection: c,
		ctx:        ctx,
	}
}

// path returns the full path of a key in the etcd namespace
func (c *collection) path(key string) string {
	return path.Join(c.prefix, key)
}

// See the documentation for `Index` for details.
func (c *collection) indexDir(index Index, indexVal string) string {
	indexDir := c.prefix
	// remove trailing slash
	indexDir = strings.TrimRight(indexDir, "/")
	return fmt.Sprintf("%s__index_%s/%s", indexDir, index, indexVal)
}

// See the documentation for `Index` for details.
func (c *collection) indexPath(index Index, indexVal string, key string) string {
	return path.Join(c.indexDir(index, indexVal), key)
}

type readWriteCollection struct {
	*collection
	stm STM
}

func (c *readWriteCollection) Get(key string, val proto.Message) error {
	valStr := c.stm.Get(c.path(key))
	if valStr == "" {
		return ErrNotFound{c.prefix, key}
	}
	return proto.UnmarshalText(valStr, val)
}

func cloneProtoMsg(original proto.Message) proto.Message {
	val := reflect.ValueOf(original)
	if val.Kind() == reflect.Ptr {
		val = reflect.Indirect(val)
	}
	return reflect.New(val.Type()).Interface().(proto.Message)
}

func (c *readWriteCollection) indexPathFromVal(val proto.Message, index Index, key string) string {
	r := reflect.ValueOf(val)
	f := reflect.Indirect(r).FieldByName(string(index)).Interface()
	indexKey := fmt.Sprintf("%s", f)
	return c.indexPath(index, indexKey, key)
}

func (c *readWriteCollection) Put(key string, val proto.Message) {
	if c.indexes != nil {
		for _, index := range c.indexes {
			indexPath := c.indexPathFromVal(val, index, key)
			clone := cloneProtoMsg(val)
			// If we can get the original value, we remove the original indexes
			if err := c.Get(key, clone); err == nil {
				originalIndexPath := c.indexPathFromVal(clone, index, key)
				if originalIndexPath != indexPath {
					c.stm.Del(originalIndexPath)
				}
			}
			// Only put the index if it doesn't already exist; otherwise
			// we might trigger an unnecessary event if someone is
			// watching the index
			if c.stm.Get(indexPath) == "" {
				c.stm.Put(indexPath, key)
			}
		}
	}
	c.stm.Put(c.path(key), val.String())
}

func (c *readWriteCollection) Create(key string, val proto.Message) error {
	fullKey := c.path(key)
	valStr := c.stm.Get(fullKey)
	if valStr != "" {
		return ErrExists{c.prefix, key}
	}
	c.Put(key, val)
	return nil
}

func (c *readWriteCollection) Delete(key string, vals ...proto.Message) error {
	fullKey := c.path(key)
	if c.stm.Get(fullKey) == "" {
		return ErrNotFound{c.prefix, key}
	}
	if c.indexes != nil && len(vals) > 0 {
		val := vals[0]
		for _, index := range c.indexes {
			// If we can get the value, we remove the corresponding indexes
			if err := c.Get(key, val); err == nil {
				indexPath := c.indexPathFromVal(val, index, key)
				c.stm.Del(indexPath)
			}
		}
	}
	c.stm.Del(fullKey)
	return nil
}

func (c *readWriteCollection) DeleteAll() {
	c.stm.DelAll(c.prefix)
}

type readWriteIntCollection struct {
	*collection
	stm STM
}

func (c *readWriteIntCollection) Create(key string, val int) error {
	fullKey := c.path(key)
	valStr := c.stm.Get(fullKey)
	if valStr != "" {
		return ErrExists{c.prefix, key}
	}
	c.stm.Put(fullKey, strconv.Itoa(val))
	return nil
}

func (c *readWriteIntCollection) Get(key string) (int, error) {
	valStr := c.stm.Get(c.path(key))
	if valStr == "" {
		return 0, ErrNotFound{c.prefix, key}
	}
	return strconv.Atoi(valStr)
}

func (c *readWriteIntCollection) Increment(key string) error {
	fullKey := c.path(key)
	valStr := c.stm.Get(fullKey)
	if valStr == "" {
		return ErrNotFound{c.prefix, key}
	}
	val, err := strconv.Atoi(valStr)
	if err != nil {
		return ErrMalformedValue{c.prefix, key, valStr}
	}
	c.stm.Put(fullKey, strconv.Itoa(val+1))
	return nil
}

func (c *readWriteIntCollection) Decrement(key string) error {
	fullKey := c.path(key)
	valStr := c.stm.Get(fullKey)
	if valStr == "" {
		return ErrNotFound{c.prefix, key}
	}
	val, err := strconv.Atoi(valStr)
	if err != nil {
		return ErrMalformedValue{c.prefix, key, valStr}
	}
	c.stm.Put(fullKey, strconv.Itoa(val-1))
	return nil
}

func (c *readWriteIntCollection) Delete(key string) error {
	fullKey := c.path(key)
	if c.stm.Get(fullKey) == "" {
		return ErrNotFound{c.prefix, key}
	}
	c.stm.Del(fullKey)
	return nil
}

type readonlyCollection struct {
	*collection
	ctx context.Context
}

func (c *readonlyCollection) Get(key string, val proto.Message) error {
	resp, err := c.etcdClient.Get(c.ctx, c.path(key))
	if err != nil {
		return err
	}

	if len(resp.Kvs) == 0 {
		return ErrNotFound{c.prefix, key}
	}

	return proto.UnmarshalText(string(resp.Kvs[0].Value), val)
}

// an indirect iterator goes through a list of keys and retrieve those
// items from the collection.
type indirectIterator struct {
	index int
	resp  *etcd.GetResponse
	col   *readonlyCollection
}

func (i *indirectIterator) Next(key *string, val proto.Message) (ok bool, retErr error) {
	if i.index < len(i.resp.Kvs) {
		kv := i.resp.Kvs[i.index]
		i.index += 1

		*key = path.Base(string(kv.Key))
		if err := i.col.Get(*key, val); err != nil {
			return false, err
		}

		return true, nil
	}
	return false, nil
}

func (c *readonlyCollection) GetByIndex(index Index, val string) (Iterator, error) {
	resp, err := c.etcdClient.Get(c.ctx, c.indexDir(index, val), etcd.WithPrefix(), etcd.WithSort(etcd.SortByModRevision, etcd.SortDescend))
	if err != nil {
		return nil, err
	}
	return &indirectIterator{
		resp: resp,
		col:  c,
	}, nil
}

// List returns an iteraor that can be used to iterate over the collection.
// The objects are sorted by revision time in descending order, i.e. newer
// objects are returned first.
func (c *readonlyCollection) List() (Iterator, error) {
	resp, err := c.etcdClient.Get(c.ctx, c.prefix, etcd.WithPrefix(), etcd.WithSort(etcd.SortByModRevision, etcd.SortDescend))
	if err != nil {
		return nil, err
	}
	return &iterator{
		resp: resp,
	}, nil
}

type iterator struct {
	index int
	resp  *etcd.GetResponse
}

func (i *iterator) Next(key *string, val proto.Message) (ok bool, retErr error) {
	if i.index < len(i.resp.Kvs) {
		kv := i.resp.Kvs[i.index]
		i.index += 1

		*key = path.Base(string(kv.Key))
		if err := proto.UnmarshalText(string(kv.Value), val); err != nil {
			return false, err
		}

		return true, nil
	}
	return false, nil
}

// Watch a collection, returning the current content of the collection as
// well as any future additions.
func (c *readonlyCollection) Watch() watch.EventChan {
	return watch.Watch(c.ctx, c.etcdClient, c.prefix)
}

// WatchByIndex watches items in a collection that match a particular index
func (c *readonlyCollection) WatchByIndex(index Index, val interface{}) watch.EventChan {
	eventCh := make(chan *watch.Event)
	indirectEventCh := watch.Watch(c.ctx, c.etcdClient, c.indexDir(index, fmt.Sprintf("%s", val)))
	go func() (retErr error) {
		defer func() {
			if retErr != nil {
				eventCh <- &watch.Event{
					Type: watch.EventError,
					Err:  retErr,
				}
				close(eventCh)
			}
		}()
		for {
			ev, ok := <-indirectEventCh
			if !ok {
				close(eventCh)
				return nil
			}
			var directEv *watch.Event
			switch ev.Type {
			case watch.EventError:
				// pass along the error
				return ev.Err
			case watch.EventPut:
				resp, err := c.etcdClient.Get(c.ctx, c.path(path.Base(string(ev.Key))))
				if err != nil {
					return err
				}
				if len(resp.Kvs) == 0 {
					// this happens only if the item was deleted shortly after
					// we receive this event.
					continue
				}
				directEv = &watch.Event{
					Key:   []byte(path.Base(string(ev.Key))),
					Value: resp.Kvs[0].Value,
					Type:  ev.Type,
				}
			case watch.EventDelete:
				directEv = &watch.Event{
					Key:  []byte(path.Base(string(ev.Key))),
					Type: ev.Type,
				}
			}
			eventCh <- directEv
		}
	}()
	return eventCh
}

// WatchOne watches a given item.  The first value returned from the watch
// will be the current value of the item.
func (c *readonlyCollection) WatchOne(key string) watch.EventChan {
	return watch.Watch(c.ctx, c.etcdClient, c.path(key))
}