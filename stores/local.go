package stores

import (
	"context"
	"encoding/json"
	"io/ioutil"
	"math/bits"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/xerrors"

	"github.com/filecoin-project/specs-actors/actors/abi"
)

type StoragePath struct {
	ID     ID
	Weight uint64

	LocalPath string

	CanSeal  bool
	CanStore bool
}

// [path]/sectorstore.json
type LocalStorageMeta struct {
	ID     ID
	Weight uint64 // 0 = readonly

	CanSeal  bool
	CanStore bool
}

// .lotusstorage/storage.json
type StorageConfig struct {
	StoragePaths []LocalPath
}

type LocalPath struct {
	Path string
}

type LocalStorage interface {
	GetStorage() (StorageConfig, error)
	SetStorage(func(*StorageConfig)) error

	Stat(path string) (FsStat, error)
}

const MetaFile = "sectorstore.json"

var PathTypes = []SectorFileType{FTUnsealed, FTSealed, FTCache}

type Local struct {
	localStorage LocalStorage
	index        SectorIndex
	urls         []string

	paths map[ID]*path

	localLk sync.RWMutex
}

type path struct {
	local string // absolute local path
}

func NewLocal(ctx context.Context, ls LocalStorage, index SectorIndex, urls []string) (*Local, error) {
	l := &Local{
		localStorage: ls,
		index:        index,
		urls:         urls,

		paths: map[ID]*path{},
	}
	return l, l.open(ctx)
}

func (st *Local) OpenPath(ctx context.Context, p string) error {
	st.localLk.Lock()
	defer st.localLk.Unlock()

	mb, err := ioutil.ReadFile(filepath.Join(p, MetaFile))
	if err != nil {
		return xerrors.Errorf("reading storage metadata for %s: %w", p, err)
	}

	var meta LocalStorageMeta
	if err := json.Unmarshal(mb, &meta); err != nil {
		return xerrors.Errorf("unmarshalling storage metadata for %s: %w", p, err)
	}

	// TODO: Check existing / dedupe

	out := &path{
		local: p,
	}

	fst, err := st.localStorage.Stat(p)
	if err != nil {
		return err
	}

	err = st.index.StorageAttach(ctx, StorageInfo{
		ID:       meta.ID,
		URLs:     st.urls,
		Weight:   meta.Weight,
		CanSeal:  meta.CanSeal,
		CanStore: meta.CanStore,
	}, fst)
	if err != nil {
		return xerrors.Errorf("declaring storage in index: %w", err)
	}

	for _, t := range PathTypes {
		ents, err := ioutil.ReadDir(filepath.Join(p, t.String()))
		if err != nil {
			if os.IsNotExist(err) {
				if err := os.MkdirAll(filepath.Join(p, t.String()), 0755); err != nil {
					return xerrors.Errorf("openPath mkdir '%s': %w", filepath.Join(p, t.String()), err)
				}

				continue
			}
			return xerrors.Errorf("listing %s: %w", filepath.Join(p, t.String()), err)
		}

		for _, ent := range ents {
			sid, err := ParseSectorID(ent.Name())
			if err != nil {
				return xerrors.Errorf("parse sector id %s: %w", ent.Name(), err)
			}

			if err := st.index.StorageDeclareSector(ctx, meta.ID, sid, t, meta.CanStore); err != nil {
				return xerrors.Errorf("declare sector %d(t:%d) -> %s: %w", sid, t, meta.ID, err)
			}
		}
	}

	st.paths[meta.ID] = out

	return nil
}

func (st *Local) open(ctx context.Context) error {
	cfg, err := st.localStorage.GetStorage()
	if err != nil {
		return xerrors.Errorf("getting local storage config: %w", err)
	}

	for _, path := range cfg.StoragePaths {
		err := st.OpenPath(ctx, path.Path)
		if err != nil {
			return xerrors.Errorf("opening path %s: %w", path.Path, err)
		}
	}

	go st.reportHealth(ctx)

	return nil
}

func (st *Local) reportHealth(ctx context.Context) {
	// randomize interval by ~10%
	interval := (HeartbeatInterval*100_000 + time.Duration(rand.Int63n(10_000))) / 100_000

	for {
		select {
		case <-time.After(interval):
		case <-ctx.Done():
			return
		}

		st.localLk.RLock()

		toReport := map[ID]HealthReport{}
		for id, p := range st.paths {
			stat, err := st.localStorage.Stat(p.local)

			toReport[id] = HealthReport{
				Stat: stat,
				Err:  err,
			}
		}

		st.localLk.RUnlock()

		for id, report := range toReport {
			if err := st.index.StorageReportHealth(ctx, id, report); err != nil {
				log.Warnf("error reporting storage health for %s: %+v", id, report)
			}
		}
	}
}

func (st *Local) AcquireSector(ctx context.Context, sid abi.SectorID, spt abi.RegisteredProof, existing SectorFileType, allocate SectorFileType, pathType PathType, op AcquireMode) (SectorPaths, SectorPaths, error) {
	if existing|allocate != existing^allocate {
		return SectorPaths{}, SectorPaths{}, xerrors.New("can't both find and allocate a sector")
	}

	st.localLk.RLock()
	defer st.localLk.RUnlock()

	var out SectorPaths
	var storageIDs SectorPaths

	for _, fileType := range PathTypes {
		if fileType&existing == 0 {
			continue
		}

		si, err := st.index.StorageFindSector(ctx, sid, fileType, false)
		if err != nil {
			log.Warnf("finding existing sector %d(t:%d) failed: %+v", sid, fileType, err)
			continue
		}

		for _, info := range si {
			p, ok := st.paths[info.ID]
			if !ok {
				continue
			}

			if p.local == "" { // TODO: can that even be the case?
				continue
			}

			spath := filepath.Join(p.local, fileType.String(), SectorName(sid))
			SetPathByType(&out, fileType, spath)
			SetPathByType(&storageIDs, fileType, string(info.ID))

			existing ^= fileType
			break
		}
	}

	for _, fileType := range PathTypes {
		if fileType&allocate == 0 {
			continue
		}

		sis, err := st.index.StorageBestAlloc(ctx, fileType, spt, pathType)
		if err != nil {
			return SectorPaths{}, SectorPaths{}, xerrors.Errorf("finding best storage for allocating : %w", err)
		}

		var best string
		var bestID ID

		for _, si := range sis {
			p, ok := st.paths[si.ID]
			if !ok {
				continue
			}

			if p.local == "" { // TODO: can that even be the case?
				continue
			}

			if (pathType == PathSealing) && !si.CanSeal {
				continue
			}

			if (pathType == PathStorage) && !si.CanStore {
				continue
			}

			// TODO: Check free space

			best = filepath.Join(p.local, fileType.String(), SectorName(sid))
			bestID = si.ID
		}

		if best == "" {
			return SectorPaths{}, SectorPaths{}, xerrors.Errorf("couldn't find a suitable path for a sector")
		}

		SetPathByType(&out, fileType, best)
		SetPathByType(&storageIDs, fileType, string(bestID))
		allocate ^= fileType
	}

	return out, storageIDs, nil
}

func (st *Local) Local(ctx context.Context) ([]StoragePath, error) {
	st.localLk.RLock()
	defer st.localLk.RUnlock()

	var out []StoragePath
	for id, p := range st.paths {
		if p.local == "" {
			continue
		}

		si, err := st.index.StorageInfo(ctx, id)
		if err != nil {
			return nil, xerrors.Errorf("get storage info for %s: %w", id, err)
		}

		out = append(out, StoragePath{
			ID:        id,
			Weight:    si.Weight,
			LocalPath: p.local,
			CanSeal:   si.CanSeal,
			CanStore:  si.CanStore,
		})
	}

	return out, nil
}

func (st *Local) Remove(ctx context.Context, sid abi.SectorID, typ SectorFileType, force bool) error {
	if bits.OnesCount(uint(typ)) != 1 {
		return xerrors.New("delete expects one file type")
	}

	si, err := st.index.StorageFindSector(ctx, sid, typ, false)
	if err != nil {
		return xerrors.Errorf("finding existing sector %d(t:%d) failed: %w", sid, typ, err)
	}

	if len(si) == 0 && !force {
		return xerrors.Errorf("can't delete sector %v(%d), not found", sid, typ)
	}

	for _, info := range si {
		if err := st.removeSector(ctx, sid, typ, info.ID); err != nil {
			return err
		}
	}

	return nil
}

func (st *Local) RemoveCopies(ctx context.Context, sid abi.SectorID, typ SectorFileType) error {
	if bits.OnesCount(uint(typ)) != 1 {
		return xerrors.New("delete expects one file type")
	}

	si, err := st.index.StorageFindSector(ctx, sid, typ, false)
	if err != nil {
		return xerrors.Errorf("finding existing sector %d(t:%d) failed: %w", sid, typ, err)
	}

	var hasPrimary bool
	for _, info := range si {
		if info.Primary {
			hasPrimary = true
			break
		}
	}

	if !hasPrimary {
		log.Warnf("RemoveCopies: no primary copies of sector %v (%s), not removing anything", sid, typ)
		return nil
	}

	for _, info := range si {
		if info.Primary {
			continue
		}

		if err := st.removeSector(ctx, sid, typ, info.ID); err != nil {
			return err
		}
	}

	return nil
}

func (st *Local) removeSector(ctx context.Context, sid abi.SectorID, typ SectorFileType, storage ID) error {
	p, ok := st.paths[storage]
	if !ok {
		return nil
	}

	if p.local == "" { // TODO: can that even be the case?
		return nil
	}

	if err := st.index.StorageDropSector(ctx, storage, sid, typ); err != nil {
		return xerrors.Errorf("dropping sector from index: %w", err)
	}

	spath := filepath.Join(p.local, typ.String(), SectorName(sid))
	log.Infof("remove %s", spath)

	if err := os.RemoveAll(spath); err != nil {
		log.Errorf("removing sector (%v) from %s: %+v", sid, spath, err)
	}

	return nil
}

func (st *Local) MoveStorage(ctx context.Context, s abi.SectorID, spt abi.RegisteredProof, types SectorFileType) error {
	dest, destIds, err := st.AcquireSector(ctx, s, spt, FTNone, types, false, AcquireMove)
	if err != nil {
		return xerrors.Errorf("acquire dest storage: %w", err)
	}

	src, srcIds, err := st.AcquireSector(ctx, s, spt, types, FTNone, false, AcquireMove)
	if err != nil {
		return xerrors.Errorf("acquire src storage: %w", err)
	}

	for _, fileType := range PathTypes {
		if fileType&types == 0 {
			continue
		}

		sst, err := st.index.StorageInfo(ctx, ID(PathByType(srcIds, fileType)))
		if err != nil {
			return xerrors.Errorf("failed to get source storage info: %w", err)
		}

		dst, err := st.index.StorageInfo(ctx, ID(PathByType(destIds, fileType)))
		if err != nil {
			return xerrors.Errorf("failed to get source storage info: %w", err)
		}

		if sst.ID == dst.ID {
			log.Debugf("not moving %v(%d); src and dest are the same", s, fileType)
			continue
		}

		if sst.CanStore {
			log.Debugf("not moving %v(%d); source supports storage", s, fileType)
			continue
		}

		log.Debugf("moving %v(%d) to storage: %s(se:%t; st:%t) -> %s(se:%t; st:%t)", s, fileType, sst.ID, sst.CanSeal, sst.CanStore, dst.ID, dst.CanSeal, dst.CanStore)

		if err := st.index.StorageDropSector(ctx, ID(PathByType(srcIds, fileType)), s, fileType); err != nil {
			return xerrors.Errorf("dropping source sector from index: %w", err)
		}

		if err := move(PathByType(src, fileType), PathByType(dest, fileType)); err != nil {
			// TODO: attempt some recovery (check if src is still there, re-declare)
			return xerrors.Errorf("moving sector %v(%d): %w", s, fileType, err)
		}

		if err := st.index.StorageDeclareSector(ctx, ID(PathByType(destIds, fileType)), s, fileType, true); err != nil {
			return xerrors.Errorf("declare sector %d(t:%d) -> %s: %w", s, fileType, ID(PathByType(destIds, fileType)), err)
		}
	}

	return nil
}

var errPathNotFound = xerrors.Errorf("fsstat: path not found")

func (st *Local) FsStat(ctx context.Context, id ID) (FsStat, error) {
	st.localLk.RLock()
	defer st.localLk.RUnlock()

	p, ok := st.paths[id]
	if !ok {
		return FsStat{}, errPathNotFound
	}

	return st.localStorage.Stat(p.local)
}

var _ Store = &Local{}
