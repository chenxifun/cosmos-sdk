package cachemulti

import (
	"context"
	"fmt"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"io"

	dbm "github.com/tendermint/tm-db"

	"github.com/cosmos/cosmos-sdk/store/cachekv"
	"github.com/cosmos/cosmos-sdk/store/dbadapter"
	"github.com/cosmos/cosmos-sdk/store/types"
)

//----------------------------------------
// Store

// Store holds many branched stores.
// Implements MultiStore.
// NOTE: a Store (and MultiStores in general) should never expose the
// keys for the substores.
type Store struct {
	db     types.CacheKVStore
	stores map[types.StoreKey]types.CacheWrap
	keys   map[string]types.StoreKey

	traceWriter  io.Writer
	traceContext types.TraceContext

	listeners map[types.StoreKey][]types.WriteListener
}

var _ types.CacheMultiStore = Store{}

// NewFromKVStore creates a new Store object from a mapping of store keys to
// CacheWrapper objects and a KVStore as the database. Each CacheWrapper store
// is a branched store.
func NewFromKVStore(
	store types.KVStore, stores map[types.StoreKey]types.CacheWrapper,
	keys map[string]types.StoreKey, traceWriter io.Writer, traceContext types.TraceContext,
	listeners map[types.StoreKey][]types.WriteListener,
) Store {
	cms := Store{
		db:           cachekv.NewStore(store),
		stores:       make(map[types.StoreKey]types.CacheWrap, len(stores)),
		keys:         keys,
		traceWriter:  traceWriter,
		traceContext: traceContext,
		listeners:    listeners,
	}

	for key, store := range stores {
		var cacheWrapped types.CacheWrap
		if cms.TracingEnabled() {
			cacheWrapped = store.CacheWrapWithTrace(cms.traceWriter, cms.traceContext)
		} else {
			cacheWrapped = store.CacheWrap()
		}
		if cms.ListeningEnabled(key) {
			cms.stores[key] = cacheWrapped.CacheWrapWithListeners(key, cms.listeners[key])
		} else {
			cms.stores[key] = cacheWrapped
		}
	}

	return cms
}

// NewStore creates a new Store object from a mapping of store keys to
// CacheWrapper objects. Each CacheWrapper store is a branched store.
func NewStore(
	db dbm.DB, stores map[types.StoreKey]types.CacheWrapper, keys map[string]types.StoreKey,
	traceWriter io.Writer, traceContext types.TraceContext, listeners map[types.StoreKey][]types.WriteListener,
) Store {

	return NewFromKVStore(dbadapter.Store{DB: db}, stores, keys, traceWriter, traceContext, listeners)
}

func newCacheMultiStoreFromCMS(cms Store) Store {
	stores := make(map[types.StoreKey]types.CacheWrapper)
	for k, v := range cms.stores {
		stores[k] = v
	}

	return NewFromKVStore(cms.db, stores, nil, cms.traceWriter, cms.traceContext, cms.listeners)
}

// SetTracer sets the tracer for the MultiStore that the underlying
// stores will utilize to trace operations. A MultiStore is returned.
func (cms Store) SetTracer(w io.Writer) types.MultiStore {
	cms.traceWriter = w
	return cms
}

// SetTracingContext updates the tracing context for the MultiStore by merging
// the given context with the existing context by key. Any existing keys will
// be overwritten. It is implied that the caller should update the context when
// necessary between tracing operations. It returns a modified MultiStore.
func (cms Store) SetTracingContext(tc types.TraceContext) types.MultiStore {
	if cms.traceContext != nil {
		for k, v := range tc {
			cms.traceContext[k] = v
		}
	} else {
		cms.traceContext = tc
	}

	return cms
}

// TracingEnabled returns if tracing is enabled for the MultiStore.
func (cms Store) TracingEnabled() bool {
	return cms.traceWriter != nil
}

// AddListeners adds listeners for a specific KVStore
func (cms Store) AddListeners(key types.StoreKey, listeners []types.WriteListener) {
	if ls, ok := cms.listeners[key]; ok {
		cms.listeners[key] = append(ls, listeners...)
	} else {
		cms.listeners[key] = listeners
	}
}

// ListeningEnabled returns if listening is enabled for a specific KVStore
func (cms Store) ListeningEnabled(key types.StoreKey) bool {
	if ls, ok := cms.listeners[key]; ok {
		return len(ls) != 0
	}
	return false
}

// GetStoreType returns the type of the store.
func (cms Store) GetStoreType() types.StoreType {
	return types.StoreTypeMulti
}

// Write calls Write on each underlying store.
func (cms Store) Write() {
	cms.SpanStart(nil, "cosmos.cms.Write", func(ctx2 context.Context, span trace.Span) {
		cms.SpanStart(ctx2, "cosmos.cms.db.Write", func(ctx3 context.Context, span trace.Span) {
			cms.db.Write()
		})
		for key, store := range cms.stores {
			cms.SpanStart(ctx2, "cosmos.cms.store.Write", func(ctx3 context.Context, span trace.Span) {
				if span != nil {
					span.SetAttributes(attribute.String("storeKey", key.String()))
				}
				store.Write()
			})
		}
	})
}

func (cms Store) SpanStart(ctx context.Context, spanName string, doFunc func(ctx2 context.Context, span trace.Span)) {
	t, ok := cms.traceContext["tracer"]
	if !ok {
		doFunc(nil, nil)
		return
	}
	if ctx == nil {
		c, ok := cms.traceContext["ctx"]
		if !ok {
			doFunc(nil, nil)
			return
		}
		ctx = c.(context.Context)
	}
	spanCtx, span := t.(trace.Tracer).Start(ctx, spanName)
	defer span.End()
	doFunc(spanCtx, span)
	return
}

// Implements CacheWrapper.
func (cms Store) CacheWrap() types.CacheWrap {
	return cms.CacheMultiStore().(types.CacheWrap)
}

// CacheWrapWithTrace implements the CacheWrapper interface.
func (cms Store) CacheWrapWithTrace(_ io.Writer, _ types.TraceContext) types.CacheWrap {
	return cms.CacheWrap()
}

// CacheWrapWithListeners implements the CacheWrapper interface.
func (cms Store) CacheWrapWithListeners(_ types.StoreKey, _ []types.WriteListener) types.CacheWrap {
	return cms.CacheWrap()
}

// Implements MultiStore.
func (cms Store) CacheMultiStore() types.CacheMultiStore {
	return newCacheMultiStoreFromCMS(cms)
}

// CacheMultiStoreWithVersion implements the MultiStore interface. It will panic
// as an already cached multi-store cannot load previous versions.
//
// TODO: The store implementation can possibly be modified to support this as it
// seems safe to load previous versions (heights).
func (cms Store) CacheMultiStoreWithVersion(_ int64) (types.CacheMultiStore, error) {
	panic("cannot branch cached multi-store with a version")
}

// GetStore returns an underlying Store by key.
func (cms Store) GetStore(key types.StoreKey) types.Store {
	s := cms.stores[key]
	if key == nil || s == nil {
		panic(fmt.Sprintf("kv store with key %v has not been registered in stores", key))
	}
	return s.(types.Store)
}

// GetKVStore returns an underlying KVStore by key.
func (cms Store) GetKVStore(key types.StoreKey) types.KVStore {
	store := cms.stores[key]
	if key == nil || store == nil {
		panic(fmt.Sprintf("kv store with key %v has not been registered in stores", key))
	}
	return store.(types.KVStore)
}
