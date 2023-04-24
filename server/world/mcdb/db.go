package mcdb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/df-mc/goleveldb/leveldb"
	"github.com/google/uuid"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
	"golang.org/x/exp/slices"
)

// DB implements a world provider for the Minecraft world format, which
// is based on a leveldb database.
type DB struct {
	conf Config
	ldb  *leveldb.DB
	dir  string
	ldat *data
	set  *world.Settings
}

// Open creates a new provider reading and writing from/to files under the path
// passed using default options. If a world is present at the path, New will
// parse its data and initialise the world with it. If the data cannot be
// parsed, an error is returned.
func Open(dir string) (*DB, error) {
	var conf Config
	return conf.Open(dir)
}

func (db *DB) LevelDat() *data {
	return db.ldat
}

// Settings returns the world.Settings of the world loaded by the DB.
func (db *DB) Settings() *world.Settings {
	return db.set
}

// SaveSettings saves the world.Settings passed to the level.dat.
func (db *DB) SaveSettings(s *world.Settings) {
	db.ldat.putSettings(s)
}

// playerData holds the fields that indicate where player data is stored for a player with a specific UUID.
type playerData struct {
	UUID         string `nbt:"MsaId"`
	ServerID     string `nbt:"ServerId"`
	SelfSignedID string `nbt:"SelfSignedId"`
}

// LoadPlayerSpawnPosition loads the players spawn position stored in the level.dat from their UUID.
func (db *DB) LoadPlayerSpawnPosition(id uuid.UUID) (pos cube.Pos, exists bool, err error) {
	serverData, _, exists, err := db.LoadPlayerData(id)
	if !exists || err != nil {
		return cube.Pos{}, exists, err
	}
	x, y, z := serverData["SpawnX"], serverData["SpawnY"], serverData["SpawnZ"]
	if x == nil || y == nil || z == nil {
		return cube.Pos{}, true, fmt.Errorf("error reading spawn fields from server data for player %v", id)
	}
	return cube.Pos{int(x.(int32)), int(y.(int32)), int(z.(int32))}, true, nil
}

// LoadPlayerData loads the data stored in a LevelDB database for a specific UUID.
func (db *DB) LoadPlayerData(id uuid.UUID) (serverData map[string]interface{}, key string, exists bool, err error) {
	data, err := db.ldb.Get([]byte("player_"+id.String()), nil)
	if err == leveldb.ErrNotFound {
		return nil, "", false, nil
	} else if err != nil {
		return nil, "", true, fmt.Errorf("error reading player data for uuid %v: %w", id, err)
	}

	var d playerData
	if err := nbt.UnmarshalEncoding(data, &d, nbt.LittleEndian); err != nil {
		return nil, "", true, fmt.Errorf("error decoding player data for uuid %v: %w", id, err)
	}
	if d.UUID != id.String() || d.ServerID == "" {
		return nil, d.ServerID, true, fmt.Errorf("invalid player data for uuid %v: %v", id, d)
	}
	serverDB, err := db.ldb.Get([]byte(d.ServerID), nil)
	if err != nil {
		return nil, d.ServerID, true, fmt.Errorf("error reading server data for player %v (%v): %w", id, d.ServerID, err)
	}

	if err := nbt.UnmarshalEncoding(serverDB, &serverData, nbt.LittleEndian); err != nil {
		return nil, d.ServerID, true, fmt.Errorf("error decoding server data for player %v", id)
	}
	return serverData, d.ServerID, true, nil
}

func (db *DB) SaveLocalPlayerData(data map[string]any) error {
	playerDataBytes, err := nbt.MarshalEncoding(data, nbt.LittleEndian)
	if err != nil {
		return fmt.Errorf("save player: error encoding nbt: %w", err)
	}

	if err := db.ldb.Put([]byte(keyLocalPlayer), playerDataBytes, nil); err != nil {
		return fmt.Errorf("save player: error Adding to db: %w", err)
	}

	return nil
}

// SavePlayerSpawnPosition saves the player spawn position passed to the levelDB database.
func (db *DB) SavePlayerSpawnPosition(id uuid.UUID, pos cube.Pos) error {
	_, err := db.ldb.Get([]byte("player_"+id.String()), nil)
	d := make(map[string]interface{})
	k := "player_server_" + id.String()

	if errors.Is(err, leveldb.ErrNotFound) {
		data, err := nbt.MarshalEncoding(playerData{
			UUID:     id.String(),
			ServerID: k,
		}, nbt.LittleEndian)
		if err != nil {
			panic(err)
		}
		if err := db.ldb.Put([]byte("player_"+id.String()), data, nil); err != nil {
			return fmt.Errorf("error writing player data for id %v: %w", id, err)
		}
	} else {
		if d, k, _, err = db.LoadPlayerData(id); err != nil {
			return err
		}
	}
	d["SpawnX"] = int32(pos.X())
	d["SpawnY"] = int32(pos.Y())
	d["SpawnZ"] = int32(pos.Z())

	data, err := nbt.MarshalEncoding(d, nbt.LittleEndian)
	if err != nil {
		panic(err)
	}
	if err = db.ldb.Put([]byte(k), data, nil); err != nil {
		return fmt.Errorf("error writing server data for player %v: %w", id, err)
	}
	return nil
}

// LoadChunk loads a chunk at the position passed from the leveldb database. If it doesn't exist, exists is
// false. If an error is returned, exists is always assumed to be true.
func (db *DB) LoadChunk(position world.ChunkPos, dim world.Dimension) (c *chunk.Chunk, exists bool, err error) {
	data := chunk.SerialisedData{}
	key := db.index(position, dim)

	// This key is where the version of a chunk resides. The chunk version has changed many times, without any
	// actual substantial changes, so we don't check this.
	_, err = db.ldb.Get(append(key, keyVersion), nil)
	if err == leveldb.ErrNotFound {
		// The new key was not found, so we try the old key.
		if _, err = db.ldb.Get(append(key, keyVersionOld), nil); err != nil {
			return nil, false, nil
		}
	} else if err != nil {
		return nil, true, fmt.Errorf("error reading version: %w", err)
	}

	data.Biomes, err = db.ldb.Get(append(key, key3DData), nil)
	if err != nil && err != leveldb.ErrNotFound {
		return nil, true, fmt.Errorf("error reading 3D data: %w", err)
	}
	if len(data.Biomes) > 512 {
		// Strip the heightmap from the biomes.
		data.Biomes = data.Biomes[512:]
	}
	data.SubChunks = make([][]byte, (dim.Range().Height()>>4)+1)
	for i := range data.SubChunks {
		data.SubChunks[i], err = db.ldb.Get(append(key, keySubChunkData, uint8(i+(dim.Range()[0]>>4))), nil)
		if err == leveldb.ErrNotFound {
			// No sub chunk present at this Y level. We skip this one and move to the next, which might still
			// be present.
			continue
		} else if err != nil {
			return nil, true, fmt.Errorf("error reading sub chunk data %v: %w", i, err)
		}
	}
	c, err = chunk.DiskDecode(data, dim.Range())
	return c, true, err
}

// SaveChunk saves a chunk at the position passed to the leveldb database. Its version is written as the
// version in the chunkVersion constant.
func (db *DB) SaveChunk(position world.ChunkPos, c *chunk.Chunk, dim world.Dimension) error {
	data := chunk.Encode(c, chunk.DiskEncoding)

	key := db.index(position, dim)
	_ = db.ldb.Put(append(key, keyVersion), []byte{chunkVersion}, nil)
	// Write the heightmap by just writing 512 empty bytes.
	_ = db.ldb.Put(append(key, key3DData), append(make([]byte, 512), data.Biomes...), nil)

	finalisation := make([]byte, 4)
	binary.LittleEndian.PutUint32(finalisation, 2)
	_ = db.ldb.Put(append(key, keyFinalisation), finalisation, nil)

	for i, sub := range data.SubChunks {
		_ = db.ldb.Put(append(key, keySubChunkData, byte(i+(c.Range()[0]>>4))), sub, nil)
	}
	return nil
}

// loadEntity loads a single entity from the map
func (db *DB) loadEntity(m map[string]any, pos world.ChunkPos, reg world.EntityRegistry) world.Entity {
	id, ok := m["identifier"]
	if !ok {
		db.conf.Log.Errorf("load entities: failed loading %v: entity had data but no identifier (%v)", pos, m)
		return nil
	}
	name, _ := id.(string)
	t, ok := reg.Lookup(name)
	if !ok {
		db.conf.Log.Errorf("load entities: failed loading %v: entity %s was not registered (%v)", pos, name, m)
		return nil
	}
	if s, ok := t.(world.SaveableEntityType); ok {
		// random UniqueID if this entity doesnt have one yet
		if _, ok := m["UniqueID"]; !ok {
			m["UniqueID"] = rand.Int63()
		}
		if v := s.DecodeNBT(m); v != nil {
			return v
		}
	}

	return nil
}

// LoadEntities loads all entities from the chunk position passed.
func (db *DB) LoadEntities(pos world.ChunkPos, dim world.Dimension, reg world.EntityRegistry) ([]world.Entity, error) {
	data, err := db.ldb.Get(append(db.index(pos, dim), keyEntities), nil)
	if err != leveldb.ErrNotFound && err != nil {
		return nil, err
	}
	var a []world.Entity

	buf := bytes.NewBuffer(data)
	dec := nbt.NewDecoderWithEncoding(buf, nbt.LittleEndian)

	for buf.Len() != 0 {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			return nil, fmt.Errorf("error decoding entity NBT: %w", err)
		}

		e := db.loadEntity(m, pos, reg)
		if e != nil {
			a = append(a, e)
		}
	}

	// load actorstorage entities
	// https://learn.microsoft.com/en-us/minecraft/creator/documents/actorstorage
	digp, err := db.ldb.Get(append([]byte("digp"), db.index(pos, dim)...), nil)
	if err == leveldb.ErrNotFound {
		return a, nil
	}
	if err != nil {
		return nil, err
	}

	for i := 0; i < len(digp); i += 8 {
		key := append([]byte("actorprefix"), digp[i:i+8]...)
		data, err := db.ldb.Get(key, nil)
		if err != leveldb.ErrNotFound && err != nil {
			return nil, err
		}
		buf := bytes.NewBuffer(data)
		dec := nbt.NewDecoderWithEncoding(buf, nbt.LittleEndian)

		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			return nil, fmt.Errorf("error decoding entity NBT: %w", err)
		}

		e := db.loadEntity(m, pos, reg)
		if e != nil {
			a = append(a, e)
		}
	}
	return a, nil
}

// SaveEntities saves all entities to the chunk position passed.
func (db *DB) SaveEntities(pos world.ChunkPos, entities []world.Entity, dim world.Dimension) error {
	digpKey := append([]byte("digp"), db.index(pos, dim)...)

	// load the ids of the previous entities
	var previousUniqueIDs []int64
	digpPrev, err := db.ldb.Get(digpKey, nil)
	if err != leveldb.ErrNotFound && err != nil {
		return err
	}
	if err != leveldb.ErrNotFound {
		for i := 0; i < len(digpPrev); i += 8 {
			uniqueID := int64(binary.LittleEndian.Uint64(digpPrev[i : i+8]))
			previousUniqueIDs = append(previousUniqueIDs, uniqueID)
		}
	}

	var uniqueIDs []int64
	for _, e := range entities {
		buf := bytes.NewBuffer(nil)
		enc := nbt.NewEncoderWithEncoding(buf, nbt.LittleEndian)
		t, ok := e.Type().(world.SaveableEntityType)
		if !ok {
			continue
		}
		x := t.EncodeNBT(e)
		x["identifier"] = t.EncodeEntity()
		if err := enc.Encode(x); err != nil {
			return fmt.Errorf("save entities: error encoding NBT: %w", err)
		}

		uniqueID, ok := x["UniqueID"].(int64)
		if !ok {
			uniqueID = rand.Int63()
		}
		if err := db.ldb.Put(db.actorIndex(uniqueID), buf.Bytes(), nil); err != nil {
			return fmt.Errorf("save entities: error Adding to db: %w", err)
		}
		uniqueIDs = append(uniqueIDs, uniqueID)
	}

	// remove entities that arent referenced anymore.
	for _, uniqueID := range previousUniqueIDs {
		if !slices.Contains(uniqueIDs, uniqueID) {
			db.ldb.Delete(db.actorIndex(uniqueID), nil)
		}
	}
	if len(entities) == 0 {
		return db.ldb.Delete(digpKey, nil)
	}

	// save the index of entities in the chunk.
	digp := make([]byte, 0, 8*len(uniqueIDs))
	for _, uniqueID := range uniqueIDs {
		digp = binary.LittleEndian.AppendUint64(digp, uint64(uniqueID))
	}
	if err := db.ldb.Put(digpKey, digp, nil); err != nil {
		return fmt.Errorf("save entities: error Adding to db: %w", err)
	}

	// remove old entity data for this chunk.
	db.ldb.Delete(append(db.index(pos, dim), keyEntities), nil)
	return nil
}

// LoadBlockNBT loads all block entities from the chunk position passed.
func (db *DB) LoadBlockNBT(position world.ChunkPos, dim world.Dimension) ([]map[string]any, error) {
	data, err := db.ldb.Get(append(db.index(position, dim), keyBlockEntities), nil)
	if err != leveldb.ErrNotFound && err != nil {
		return nil, err
	}
	var a []map[string]any

	buf := bytes.NewBuffer(data)
	dec := nbt.NewDecoderWithEncoding(buf, nbt.LittleEndian)

	for buf.Len() != 0 {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			return nil, fmt.Errorf("error decoding block NBT: %w", err)
		}
		a = append(a, m)
	}
	return a, nil
}

// SaveBlockNBT saves all block NBT data to the chunk position passed.
func (db *DB) SaveBlockNBT(position world.ChunkPos, data []map[string]any, dim world.Dimension) error {
	if len(data) == 0 {
		return db.ldb.Delete(append(db.index(position, dim), keyBlockEntities), nil)
	}
	buf := bytes.NewBuffer(nil)
	enc := nbt.NewEncoderWithEncoding(buf, nbt.LittleEndian)
	for _, d := range data {
		if err := enc.Encode(d); err != nil {
			return fmt.Errorf("error encoding block NBT: %w", err)
		}
	}
	return db.ldb.Put(append(db.index(position, dim), keyBlockEntities), buf.Bytes(), nil)
}

// NewChunkIterator returns a ChunkIterator that may be used to iterate over all
// position/chunk pairs in a database.
// An IteratorRange r may be passed to specify limits in terms of what chunks
// should be read. r may be set to nil to read all chunks from the DB.
func (db *DB) NewChunkIterator(r *IteratorRange) *ChunkIterator {
	if r == nil {
		r = &IteratorRange{}
	}
	return newChunkIterator(db, r)
}

// Close closes the provider, saving any file that might need to be saved, such as the level.dat.
func (db *DB) Close() error {
	db.ldat.LastPlayed = time.Now().Unix()

	buf := bytes.NewBuffer(nil)
	if err := db.ldat.marshal(buf); err != nil {
		return fmt.Errorf("encode level.dat: %w", err)
	}
	if err := os.WriteFile(filepath.Join(db.dir, "level.dat"), buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("error writing levelname.txt: %w", err)
	}
	if err := os.WriteFile(filepath.Join(db.dir, "levelname.txt"), []byte(db.ldat.LevelName), 0644); err != nil {
		return fmt.Errorf("error writing levelname.txt: %w", err)
	}
	return db.ldb.Close()
}

func (db *DB) actorIndex(uniqueID int64) []byte {
	return binary.LittleEndian.AppendUint64([]byte("actorprefix"), uint64(uniqueID))
}

// index returns a byte buffer holding the written index of the chunk position passed. If the dimension passed
// is not world.Overworld, the length of the index returned is 12. It is 8 otherwise.
func (db *DB) index(position world.ChunkPos, d world.Dimension) []byte {
	dim, _ := world.DimensionID(d)
	x, z := uint32(position[0]), uint32(position[1])
	b := make([]byte, 12)

	binary.LittleEndian.PutUint32(b, x)
	binary.LittleEndian.PutUint32(b[4:], z)
	if dim == 0 {
		return b[:8]
	}
	binary.LittleEndian.PutUint32(b[8:], uint32(dim))
	return b
}

func (db *DB) LDB() *leveldb.DB {
	return db.ldb
}
