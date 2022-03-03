// Copyright 2019-2022 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// This file incorporates work covered by the following copyright and
// permission notice:
//
// Copyright 2016 Attic Labs, Inc. All rights reserved.
// Licensed under the Apache License, version 2.0:
// http://www.apache.org/licenses/LICENSE-2.0

package datas

import (
	"context"
	"errors"
	"fmt"

	"github.com/dolthub/dolt/go/store/chunks"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/types"
)

type database struct {
	*types.ValueStore
	rt rootTracker
}

var (
	ErrOptimisticLockFailed = errors.New("optimistic lock failed on database Root update")
	ErrMergeNeeded          = errors.New("dataset head is not ancestor of commit")
	ErrAlreadyCommitted     = errors.New("dataset head already pointing at given commit")
)

// rootTracker is a narrowing of the ChunkStore interface, to keep Database disciplined about working directly with Chunks
type rootTracker interface {
	Root(ctx context.Context) (hash.Hash, error)
	Commit(ctx context.Context, current, last hash.Hash) (bool, error)
}

func newDatabase(vs *types.ValueStore) *database {
	return &database{
		ValueStore: vs, // ValueStore is responsible for closing |cs|
		rt:         vs,
	}
}

var _ Database = &database{}
var _ GarbageCollector = &database{}

var _ rootTracker = &types.ValueStore{}
var _ GarbageCollector = &types.ValueStore{}

func (db *database) chunkStore() chunks.ChunkStore {
	return db.ChunkStore()
}

func (db *database) Stats() interface{} {
	return db.ChunkStore().Stats()
}

func (db *database) StatsSummary() string {
	return db.ChunkStore().StatsSummary()
}

// DatasetsInRoot returns the Map of datasets in the root represented by the |rootHash| given
func (db *database) loadDatasetsNomsMap(ctx context.Context, rootHash hash.Hash) (types.Map, error) {
	if rootHash.IsEmpty() {
		return types.NewMap(ctx, db)
	}

	val, err := db.ReadValue(ctx, rootHash)
	if err != nil {
		return types.EmptyMap, err
	}

	return val.(types.Map), nil
}

func (db *database) loadDatasetsRefmap(ctx context.Context, rootHash hash.Hash) (refmap, error) {
	if rootHash == (hash.Hash{}) {
		return refmap{}, nil
	}

	chunk, err := db.chunkStore().Get(ctx, rootHash)
	if err != nil {
		return refmap{}, err
	}
	if chunk.IsEmpty() {
		return refmap{}, errors.New("root hash not found in database")
	}

	return parse_storeroot(chunk.Data()), nil
}

func getParentsClosure(ctx context.Context, vrw types.ValueReadWriter, parentRefsL types.List) (types.Ref, bool, error) {
	parentRefs := make([]types.Ref, int(parentRefsL.Len()))
	parents := make([]types.Struct, len(parentRefs))
	if len(parents) == 0 {
		return types.Ref{}, false, nil
	}
	err := parentRefsL.IterAll(ctx, func(v types.Value, i uint64) error {
		r, ok := v.(types.Ref)
		if !ok {
			return errors.New("parentsRef element was not a Ref")
		}
		parentRefs[int(i)] = r
		tv, err := r.TargetValue(ctx, vrw)
		if err != nil {
			return err
		}
		s, ok := tv.(types.Struct)
		if !ok {
			return errors.New("parentRef target value was not a Struct")
		}
		parents[int(i)] = s
		return nil
	})
	if err != nil {
		return types.Ref{}, false, err
	}
	parentMaps := make([]types.Map, len(parents))
	parentParentLists := make([]types.List, len(parents))
	for i, p := range parents {
		v, ok, err := p.MaybeGet(ParentsClosureField)
		if err != nil {
			return types.Ref{}, false, err
		}
		if !ok || types.IsNull(v) {
			empty, err := types.NewMap(ctx, vrw)
			if err != nil {
				return types.Ref{}, false, err
			}
			parentMaps[i] = empty
		} else {
			r, ok := v.(types.Ref)
			if !ok {
				return types.Ref{}, false, errors.New("unexpected field value type for parents_closure in commit struct")
			}
			tv, err := r.TargetValue(ctx, vrw)
			if err != nil {
				return types.Ref{}, false, err
			}
			parentMaps[i], ok = tv.(types.Map)
			if !ok {
				return types.Ref{}, false, fmt.Errorf("unexpected target value type for parents_closure in commit struct: %v", tv)
			}
		}
		v, ok, err = p.MaybeGet(ParentsListField)
		if err != nil {
			return types.Ref{}, false, err
		}
		if !ok || types.IsNull(v) {
			empty, err := types.NewList(ctx, vrw)
			if err != nil {
				return types.Ref{}, false, err
			}
			parentParentLists[i] = empty
		} else {
			parentParentLists[i], ok = v.(types.List)
			if !ok {
				return types.Ref{}, false, errors.New("unexpected field value or type for parents_list in commit struct")
			}
		}
		if parentMaps[i].Len() == 0 && parentParentLists[i].Len() != 0 {
			// If one of the commits has an empty parents_closure, but non-empty parents, we will not record
			// a parents_closure here.
			return types.Ref{}, false, nil
		}
	}
	// Convert parent lists to List<Ref<Value>>
	for i, l := range parentParentLists {
		newRefs := make([]types.Value, int(l.Len()))
		err := l.IterAll(ctx, func(v types.Value, i uint64) error {
			r, ok := v.(types.Ref)
			if !ok {
				return errors.New("unexpected entry type for parents_list in commit struct")
			}
			newRefs[int(i)], err = types.ToRefOfValue(r, vrw.Format())
			if err != nil {
				return err
			}
			return nil
		})
		if err != nil {
			return types.Ref{}, false, err
		}
		parentParentLists[i], err = types.NewList(ctx, vrw, newRefs...)
		if err != nil {
			return types.Ref{}, false, err
		}
	}
	editor := parentMaps[0].Edit()
	for i, r := range parentRefs {
		h := r.TargetHash()
		key, err := types.NewTuple(vrw.Format(), types.Uint(r.Height()), types.InlineBlob(h[:]))
		if err != nil {
			editor.Close(ctx)
			return types.Ref{}, false, err
		}
		editor.Set(key, parentParentLists[i])
	}
	for i := 1; i < len(parentMaps); i++ {
		changes := make(chan types.ValueChanged)
		var derr error
		go func() {
			defer close(changes)
			derr = parentMaps[1].Diff(ctx, parentMaps[0], changes)
		}()
		for c := range changes {
			if c.ChangeType == types.DiffChangeAdded {
				editor.Set(c.Key, c.NewValue)
			}
		}
		if derr != nil {
			editor.Close(ctx)
			return types.Ref{}, false, derr
		}
	}
	m, err := editor.Map(ctx)
	if err != nil {
		return types.Ref{}, false, err
	}
	r, err := vrw.WriteValue(ctx, m)
	if err != nil {
		return types.Ref{}, false, err
	}
	r, err = types.ToRefOfValue(r, vrw.Format())
	if err != nil {
		return types.Ref{}, false, err
	}
	return r, true, nil
}

type refmapDatasetsMap struct {
	rm refmap
}

func (m refmapDatasetsMap) Len() uint64 {
	return uint64(len(m.rm.entries))
}

func (m refmapDatasetsMap) IterAll(ctx context.Context, cb func(string, hash.Hash) error) error {
	for _, e := range m.rm.entries {
		if err := cb(e.name, e.addr); err != nil {
			return err
		}
	}
	return nil
}

type nomsDatasetsMap struct {
	m types.Map
}

func (m nomsDatasetsMap) Len() uint64 {
	return m.m.Len()
}

func (m nomsDatasetsMap) IterAll(ctx context.Context, cb func(string, hash.Hash) error) error {
	return m.m.IterAll(ctx, func(k, v types.Value) error {
		// TODO: very fast and loose with error checking here.
		return cb(string(k.(types.String)), v.(types.Ref).TargetHash())
	})
}

// Datasets returns the Map of Datasets in the current root. If you intend to edit the map and commit changes back,
// then you should fetch the current root, then call DatasetsInRoot with that hash. Otherwise another writer could
// change the root value between when you get the root hash and call this method.
func (db *database) Datasets(ctx context.Context) (DatasetsMap, error) {
	rootHash, err := db.rt.Root(ctx)
	if err != nil {
		return nil, err
	}

	if db.Format() == types.Format_DOLT_1 {
		rm, err := db.loadDatasetsRefmap(ctx, rootHash)
		if err != nil {
			return nil, err
		}
		return refmapDatasetsMap{rm}, nil
	}

	m, err := db.loadDatasetsNomsMap(ctx, rootHash)
	if err != nil {
		return nil, err
	}

	return nomsDatasetsMap{m}, nil
}

var ErrInvalidDatasetID = errors.New("Invalid dataset ID")

func (db *database) GetDataset(ctx context.Context, datasetID string) (Dataset, error) {
	// precondition checks
	if !DatasetFullRe.MatchString(datasetID) {
		return Dataset{}, fmt.Errorf("%w: %s", ErrInvalidDatasetID, datasetID)
	}

	datasets, err := db.Datasets(ctx)
	if err != nil {
		return Dataset{}, err
	}

	return db.datasetFromMap(ctx, datasetID, datasets)
}

func (db *database) datasetFromMap(ctx context.Context, datasetID string, dsmap DatasetsMap) (Dataset, error) {
	if ndsmap, ok := dsmap.(nomsDatasetsMap); ok {
		datasets := ndsmap.m
		var headChunk chunks.Chunk
		if r, ok, err := datasets.MaybeGet(ctx, types.String(datasetID)); err != nil {
			return Dataset{}, err
		} else if ok {
			headChunk, err = db.chunkStore().Get(ctx, r.(types.Ref).TargetHash())
			if err != nil {
				return Dataset{}, err
			}
		}
		return newDataset(db, datasetID, headChunk)
	} else if rmdsmap, ok := dsmap.(refmapDatasetsMap); ok {
		var headChunk chunks.Chunk
		var err error
		curr := rmdsmap.rm.lookup(datasetID)
		if !curr.IsEmpty() {
			headChunk, err = db.chunkStore().Get(ctx, curr)
			if err != nil {
				return Dataset{}, err
			}
		}
		return newDataset(db, datasetID, headChunk)
	} else {
		return Dataset{}, errors.New("unimplemented or unsupported DatasetsMap type")
	}
}

func (db *database) readHead(ctx context.Context, addr hash.Hash) (dsHead, error) {
	err := db.Flush(ctx)
	if err != nil {
		return nil, err
	}
	headChunk, err := db.chunkStore().Get(ctx, addr)
	if err != nil {
		return nil, err
	}
	return newHead(db, headChunk)
}

func (db *database) Close() error {
	return db.ValueStore.Close()
}

func (db *database) SetHead(ctx context.Context, ds Dataset, newHeadAddr hash.Hash) (Dataset, error) {
	return db.doHeadUpdate(ctx, ds, func(ds Dataset) error { return db.doSetHead(ctx, ds, newHeadAddr) })
}

func (db *database) doSetHead(ctx context.Context, ds Dataset, addr hash.Hash) error {
	newHead, err := db.readHead(ctx, addr)
	if err != nil {
		return err
	}

	newSt := newHead.(nomsHead).st

	headType := newHead.TypeName()
	switch headType {
	case CommitName:
		var iscommit bool
		iscommit, err = IsCommit(newSt)
		if err != nil {
			break
		}
		if !iscommit {
			err = fmt.Errorf("SetHead failed: reffered to value is not a commit:")
		}
	case TagName:
		err = db.validateTag(ctx, newHead.(nomsHead).st)
	default:
		return fmt.Errorf("Unrecognized dataset value: %s", headType)
	}
	if err != nil {
		return err
	}

	key := types.String(ds.ID())

	vref, err := types.NewRef(newSt, db.Format())
	if err != nil {
		return err
	}

	ref, err := types.ToRefOfValue(vref, db.Format())
	if err != nil {
		return err
	}

	return db.update(ctx, func(ctx context.Context, datasets types.Map) (types.Map, error) {
		currRef, ok, err := datasets.MaybeGet(ctx, key)
		if err != nil {
			return types.Map{}, err
		}
		if ok {
			currSt, err := currRef.(types.Ref).TargetValue(ctx, db)
			if err != nil {
				return types.Map{}, err
			}
			currType := currSt.(types.Struct).Name()
			if currType != headType {
				return types.Map{}, fmt.Errorf("cannot change type of head; currently points at %s but new value would point at %s", currType, headType)
			}
		}

		return datasets.Edit().Set(key, ref).Map(ctx)
	}, func(ctx context.Context, rm refmap) (refmap, error) {
		curr := rm.lookup(ds.ID())
		if curr != (hash.Hash{}) {
			currHead, err := db.readHead(ctx, curr)
			if err != nil {
				return refmap{}, err
			}
			currType := currHead.TypeName()
			if currType != headType {
				return refmap{}, fmt.Errorf("cannot change type of head; currently points at %s but new value would point at %s", currType, headType)
			}
		}
		rm.set(ds.ID(), ref.TargetHash())
		return rm, nil
	})
}

func (db *database) FastForward(ctx context.Context, ds Dataset, newHeadAddr hash.Hash) (Dataset, error) {
	return db.doHeadUpdate(ctx, ds, func(ds Dataset) error { return db.doFastForward(ctx, ds, newHeadAddr) })
}

func (db *database) doFastForward(ctx context.Context, ds Dataset, newHeadAddr hash.Hash) error {
	newHead, err := db.readHead(ctx, newHeadAddr)
	if err != nil {
		return err
	}
	if newHead == nil {
		return fmt.Errorf("FastForward: new head address %v not found", newHeadAddr)
	}
	if newHead.TypeName() != CommitName {
		return fmt.Errorf("FastForward: target value of new head address %v is not a commit.", newHeadAddr)
	}

	var v types.Value
	v = newHead.(nomsHead).st
	iscommit, err := IsCommit(v)
	if err != nil {
		return err
	}
	if !iscommit {
		return fmt.Errorf("FastForward: target value of new head address %v is not a commit.", newHeadAddr)
	}

	newRef, err := types.NewRef(v, db.Format())
	if err != nil {
		return err
	}

	newValueRef, err := types.ToRefOfValue(newRef, db.Format())
	if err != nil {
		return err
	}

	var currentHeadAddr hash.Hash
	if ref, ok, err := ds.MaybeHeadRef(); err != nil {
		return err
	} else if ok {
		currentHeadAddr = ref.TargetHash()
		ancestorRef, found, err := FindCommonAncestor(ctx, ref, newRef, db, db)
		if err != nil {
			return err
		}
		if !found || mergeNeeded(ref, ancestorRef, newRef) {
			return ErrMergeNeeded
		}
	}

	err = db.doCommit(ctx, ds.ID(), currentHeadAddr, newValueRef)
	if err == ErrAlreadyCommitted {
		return nil
	}
	return err
}

func (db *database) Commit(ctx context.Context, ds Dataset, v types.Value, opts CommitOptions) (Dataset, error) {
	var currentAddr hash.Hash
	if currRef, ok, err := ds.MaybeHeadRef(); err != nil {
		return Dataset{}, err
	} else if ok {
		currentAddr = currRef.TargetHash()
	}
	st, err := buildNewCommit(ctx, ds, v, opts)
	if err != nil {
		return Dataset{}, err
	}

	commitRef, err := db.WriteValue(ctx, st)
	if err != nil {
		return Dataset{}, err
	}

	newCommitValueRef, err := types.ToRefOfValue(commitRef, db.Format())
	if err != nil {
		return Dataset{}, err
	}

	return db.doHeadUpdate(
		ctx,
		ds,
		func(ds Dataset) error {
			return db.doCommit(ctx, ds.ID(), currentAddr, newCommitValueRef)
		},
	)
}

// Calls db.Commit with empty CommitOptions{}.
func CommitValue(ctx context.Context, db Database, ds Dataset, v types.Value) (Dataset, error) {
	return db.Commit(ctx, ds, v, CommitOptions{})
}

func (db *database) doCommit(ctx context.Context, datasetID string, datasetCurrentAddr hash.Hash, newCommitValueRef types.Ref) error {
	return db.update(ctx, func(ctx context.Context, datasets types.Map) (types.Map, error) {
		curr, hasHead, err := datasets.MaybeGet(ctx, types.String(datasetID))
		if err != nil {
			return types.Map{}, err
		}
		if hasHead {
			currRef := curr.(types.Ref)
			if currRef.TargetHash() != datasetCurrentAddr {
				return types.Map{}, ErrMergeNeeded
			}
			if currRef.TargetHash() == newCommitValueRef.TargetHash() {
				return types.Map{}, ErrAlreadyCommitted
			}
		} else if datasetCurrentAddr != (hash.Hash{}) {
			return types.Map{}, ErrMergeNeeded
		}

		return datasets.Edit().Set(types.String(datasetID), newCommitValueRef).Map(ctx)
	}, func(ctx context.Context, rm refmap) (refmap, error) {
		curr := rm.lookup(datasetID)
		if curr != datasetCurrentAddr {
			return refmap{}, ErrMergeNeeded
		}
		if curr != (hash.Hash{}) {
			if curr == newCommitValueRef.TargetHash() {
				return refmap{}, ErrAlreadyCommitted
			}
		}
		rm.set(datasetID, newCommitValueRef.TargetHash())
		return rm, nil
	})
}

func mergeNeeded(currentHeadRef types.Ref, ancestorRef types.Ref, commitRef types.Ref) bool {
	return currentHeadRef.TargetHash() != ancestorRef.TargetHash()
}

func (db *database) Tag(ctx context.Context, ds Dataset, commitAddr hash.Hash, opts TagOptions) (Dataset, error) {
	return db.doHeadUpdate(
		ctx,
		ds,
		func(ds Dataset) error {
			addr, tagRef, err := newTag(ctx, db, commitAddr, opts.Meta)
			if err != nil {
				return err
			}
			return db.doTag(ctx, ds.ID(), addr, tagRef)
		},
	)
}

// doTag manages concurrent access the single logical piece of mutable state: the current Root. It uses
// the same optimistic writing algorithm as doCommit (see above).
func (db *database) doTag(ctx context.Context, datasetID string, tagAddr hash.Hash, tagRef types.Ref) error {
	return db.update(ctx, func(ctx context.Context, datasets types.Map) (types.Map, error) {
		_, hasHead, err := datasets.MaybeGet(ctx, types.String(datasetID))
		if err != nil {
			return types.Map{}, err
		}
		if hasHead {
			return types.Map{}, fmt.Errorf("tag %s already exists and cannot be altered after creation", datasetID)
		}

		return datasets.Edit().Set(types.String(datasetID), tagRef).Map(ctx)
	}, func(ctx context.Context, rm refmap) (refmap, error) {
		curr := rm.lookup(datasetID)
		if curr != (hash.Hash{}) {
			fmt.Errorf("tag %s already exists and cannot be altered after creation", datasetID)
		}
		rm.set(datasetID, tagAddr)
		return rm, nil
	})
}

func (db *database) UpdateWorkingSet(ctx context.Context, ds Dataset, workingSet WorkingSetSpec, prevHash hash.Hash) (Dataset, error) {
	return db.doHeadUpdate(
		ctx,
		ds,
		func(ds Dataset) error {
			addr, ref, err := newWorkingSet(ctx, db, workingSet.Meta, workingSet.WorkingRoot, workingSet.StagedRoot, workingSet.MergeState)
			if err != nil {
				return err
			}
			return db.doUpdateWorkingSet(ctx, ds.ID(), addr, ref, prevHash)
		},
	)
}

// Update the entry in the datasets map for |datasetID| to point to a ref of
// |workingSet|. Unlike |doCommit|, |doTag|, etc., this method requires a
// compare-and-set for the current target hash of the datasets entry, and will
// return an error if the application is working with a stale value for the
// workingset.
func (db *database) doUpdateWorkingSet(ctx context.Context, datasetID string, addr hash.Hash, ref types.Ref, currHash hash.Hash) error {
	return db.update(ctx, func(ctx context.Context, datasets types.Map) (types.Map, error) {
		success, err := assertDatasetHash(ctx, datasets, datasetID, currHash)
		if err != nil {
			return types.Map{}, err
		}
		if !success {
			return types.Map{}, ErrOptimisticLockFailed
		}

		return datasets.Edit().Set(types.String(datasetID), ref).Map(ctx)
	}, func(ctx context.Context, rm refmap) (refmap, error) {
		curr := rm.lookup(datasetID)
		if curr != currHash {
			return refmap{}, ErrOptimisticLockFailed
		}
		rm.set(datasetID, addr)
		return rm, nil
	})
}

// assertDatasetHash returns true if the hash of the dataset matches the one given. Use an empty hash for a dataset you
// expect not to exist.
// Typically this is called using optimistic locking by the caller in order to implement atomic test-and-set semantics.
func assertDatasetHash(
	ctx context.Context,
	datasets types.Map,
	datasetID string,
	currHash hash.Hash,
) (bool, error) {
	curr, ok, err := datasets.MaybeGet(ctx, types.String(datasetID))
	if err != nil {
		return false, err
	}
	if !ok {
		return currHash.IsEmpty(), nil
	}
	return curr.(types.Ref).TargetHash().Equal(currHash), nil
}

// CommitWithWorkingSet updates two Datasets atomically: the working set, and its corresponding HEAD. Uses the same
// global locking mechanism as UpdateWorkingSet.
func (db *database) CommitWithWorkingSet(
	ctx context.Context,
	commitDS, workingSetDS Dataset,
	val types.Value, workingSetSpec WorkingSetSpec,
	prevWsHash hash.Hash, opts CommitOptions,
) (Dataset, Dataset, error) {
	wsAddr, wsValRef, err := newWorkingSet(ctx, db, workingSetSpec.Meta, workingSetSpec.WorkingRoot, workingSetSpec.StagedRoot, workingSetSpec.MergeState)
	if err != nil {
		return Dataset{}, Dataset{}, err
	}

	commit, err := buildNewCommit(ctx, commitDS, val, opts)
	if err != nil {
		return Dataset{}, Dataset{}, err
	}

	commitRef, err := db.WriteValue(ctx, commit)
	if err != nil {
		return Dataset{}, Dataset{}, err
	}

	commitValRef, err := types.ToRefOfValue(commitRef, db.Format())
	if err != nil {
		return Dataset{}, Dataset{}, err
	}

	var currDSHash hash.Hash
	currDSRef, ok, err := commitDS.MaybeHeadRef()
	if err != nil {
		return Dataset{}, Dataset{}, err
	}
	if ok {
		currDSHash = currDSRef.TargetHash()
	}

	err = db.update(ctx, func(ctx context.Context, datasets types.Map) (types.Map, error) {
		success, err := assertDatasetHash(ctx, datasets, workingSetDS.ID(), prevWsHash)
		if err != nil {
			return types.Map{}, err
		}

		if !success {
			return types.Map{}, ErrOptimisticLockFailed
		}

		r, hasHead, err := datasets.MaybeGet(ctx, types.String(commitDS.ID()))
		if err != nil {
			return types.Map{}, err
		}

		if hasHead {
			currentHeadRef := r.(types.Ref)
			ancestorRef, found, err := FindCommonAncestor(ctx, commitRef, currentHeadRef, db, db)
			if err != nil {
				return types.Map{}, err
			}

			if !found || mergeNeeded(currentHeadRef, ancestorRef, commitRef) {
				return types.Map{}, ErrMergeNeeded
			}
		}

		return datasets.Edit().
			Set(types.String(workingSetDS.ID()), wsValRef).
			Set(types.String(commitDS.ID()), commitValRef).
			Map(ctx)
	}, func(ctx context.Context, rm refmap) (refmap, error) {
		currWS := rm.lookup(workingSetDS.ID())
		if currWS != prevWsHash {
			return refmap{}, ErrOptimisticLockFailed
		}
		currDS := rm.lookup(commitDS.ID())
		if currDS != currDSHash {
			return refmap{}, ErrMergeNeeded
		}
		rm.set(commitDS.ID(), commitValRef.TargetHash())
		rm.set(workingSetDS.ID(), wsAddr)
		return rm, nil
	})

	if err != nil {
		return Dataset{}, Dataset{}, err
	}

	currentDatasets, err := db.Datasets(ctx)
	if err != nil {
		return Dataset{}, Dataset{}, err
	}

	commitDS, err = db.datasetFromMap(ctx, commitDS.ID(), currentDatasets)
	if err != nil {
		return Dataset{}, Dataset{}, err
	}

	workingSetDS, err = db.datasetFromMap(ctx, workingSetDS.ID(), currentDatasets)
	if err != nil {
		return Dataset{}, Dataset{}, err
	}

	return commitDS, workingSetDS, nil
}

func (db *database) Delete(ctx context.Context, ds Dataset) (Dataset, error) {
	return db.doHeadUpdate(ctx, ds, func(ds Dataset) error { return db.doDelete(ctx, ds.ID()) })
}

func (db *database) update(ctx context.Context,
	edit func(context.Context, types.Map) (types.Map, error),
	editFB func(context.Context, refmap) (refmap, error)) error {
	var (
		err      error
		root     hash.Hash
		datasets types.Map
	)
	for {
		root, err = db.rt.Root(ctx)
		if err != nil {
			return err
		}

		var newRootHash hash.Hash

		if db.Format() == types.Format_DOLT_1 {
			datasets, err := db.loadDatasetsRefmap(ctx, root)
			if err != nil {
				return err
			}

			datasets, err = editFB(ctx, datasets)
			if err != nil {
				return err
			}

			data := datasets.storeroot_flatbuffer()
			chunk := chunks.NewChunk(data)
			err = db.chunkStore().Put(ctx, chunk)
			if err != nil {
				return err
			}

			newRootHash = chunk.Hash()
		} else {
			datasets, err = db.loadDatasetsNomsMap(ctx, root)
			if err != nil {
				return err
			}

			datasets, err = edit(ctx, datasets)
			if err != nil {
				return err
			}

			newRoot, err := db.WriteValue(ctx, datasets)
			if err != nil {
				return err
			}

			newRootHash = newRoot.TargetHash()
		}

		err = db.tryCommitChunks(ctx, newRootHash, root)
		if err != ErrOptimisticLockFailed {
			break
		}
	}
	return err
}

func (db *database) doDelete(ctx context.Context, datasetIDstr string) error {
	var first types.Value
	var firstHash hash.Hash

	datasetID := types.String(datasetIDstr)
	return db.update(ctx, func(ctx context.Context, datasets types.Map) (types.Map, error) {
		curr, ok, err := datasets.MaybeGet(ctx, datasetID)
		if err != nil {
			return types.Map{}, err
		} else if !ok {
			if first != nil {
				return types.Map{}, ErrMergeNeeded
			}
			return datasets, nil
		} else if first == nil {
			first = curr
		} else if !first.Equals(curr) {
			return types.Map{}, ErrMergeNeeded
		}
		return datasets.Edit().Remove(datasetID).Map(ctx)
	}, func(ctx context.Context, rm refmap) (refmap, error) {
		curr := rm.lookup(datasetIDstr)
		if curr != (hash.Hash{}) && firstHash == (hash.Hash{}) {
			firstHash = curr
		}
		if curr != firstHash {
			return refmap{}, ErrMergeNeeded
		}
		rm.delete(datasetIDstr)
		return rm, nil
	})
}

// GC traverses the database starting at the Root and removes all unreferenced data from persistent storage.
func (db *database) GC(ctx context.Context, oldGenRefs, newGenRefs hash.HashSet) error {
	return db.ValueStore.GC(ctx, oldGenRefs, newGenRefs)
}

func (db *database) tryCommitChunks(ctx context.Context, newRootHash hash.Hash, currentRootHash hash.Hash) error {
	if success, err := db.rt.Commit(ctx, newRootHash, currentRootHash); err != nil {
		return err
	} else if !success {
		return ErrOptimisticLockFailed
	}
	return nil
}

func (db *database) validateRefAsCommit(ctx context.Context, r types.Ref) (types.Struct, error) {
	rHead, err := db.readHead(ctx, r.TargetHash())
	if err != nil {
		return types.Struct{}, err
	}
	if rHead == nil {
		panic("wat")
		return types.Struct{}, fmt.Errorf("validateRefAsCommit: unable to validate ref; %s not found", r.TargetHash().String())
	}
	if rHead.TypeName() != CommitName {
		return types.Struct{}, fmt.Errorf("validateRefAsCommit: referred valus is not a commit")
	}

	var v types.Value
	v = rHead.(nomsHead).st

	is, err := IsCommit(v)

	if err != nil {
		return types.Struct{}, err
	}

	if !is {
		return types.Struct{}, fmt.Errorf("validateRefAsCommit: referred valus is not a commit")
	}

	return v.(types.Struct), nil
}

func (db *database) validateTag(ctx context.Context, t types.Struct) error {
	is, err := IsTag(t)
	if err != nil {
		return err
	}
	if !is {
		return fmt.Errorf("Tag struct %s is malformed, IsTag() == false", t.String())
	}

	r, ok, err := t.MaybeGet(TagCommitRefField)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("tag is missing field %s", TagCommitRefField)
	}

	_, err = db.validateRefAsCommit(ctx, r.(types.Ref))

	if err != nil {
		return err
	}

	return nil
}

func (db *database) validateWorkingSet(t types.Struct) error {
	is, err := IsWorkingSet(t)
	if err != nil {
		return err
	}
	if !is {
		return fmt.Errorf("WorkingSet struct %s is malformed, IsWorkingSet() == false", t.String())
	}

	_, ok, err := t.MaybeGet(WorkingRootRefField)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("WorkingSet is missing field %s", WorkingRootRefField)
	}

	return nil
}

func buildNewCommit(ctx context.Context, ds Dataset, v types.Value, opts CommitOptions) (types.Struct, error) {
	parents := opts.ParentsList
	if parents == types.EmptyList || parents.Len() == 0 {
		var err error
		parents, err = types.NewList(ctx, ds.db)
		if err != nil {
			return types.Struct{}, err
		}

		if headRef, ok, err := ds.MaybeHeadRef(); err != nil {
			return types.Struct{}, err
		} else if ok {
			le := parents.Edit().Append(headRef)
			parents, err = le.List(ctx)
			if err != nil {
				return types.Struct{}, err
			}
		}
	} else {
		curr, ok, err := ds.MaybeHeadRef()
		if err != nil {
			return types.Struct{}, err
		}
		if ok {
			var found bool
			err := parents.IterAll(ctx, func(v types.Value, _ uint64) error {
				if v.(types.Ref).TargetHash() == curr.TargetHash() {
					found = true
				}
				return nil
			})
			if err != nil {
				return types.Struct{}, err
			}
			if !found {
				return types.Struct{}, ErrMergeNeeded
			}
		}
	}

	meta := opts.Meta
	if meta.IsZeroValue() {
		meta = types.EmptyStruct(ds.db.Format())
	}

	parentsClosure, includeParentsClosure, err := getParentsClosure(ctx, ds.db, parents)
	if err != nil {
		return types.Struct{}, err
	}

	return newCommit(ctx, v, parents, parentsClosure, includeParentsClosure, meta)
}

func (db *database) doHeadUpdate(ctx context.Context, ds Dataset, updateFunc func(ds Dataset) error) (Dataset, error) {
	err := updateFunc(ds)
	if err != nil {
		return Dataset{}, err
	}

	return db.GetDataset(ctx, ds.ID())
}
