package world

import (
	"bytes"
	_ "embed"
	"fmt"
	"hash/fnv"
	"image/color"
	"math"
	"sort"
	"strings"
	"unsafe"

	"github.com/df-mc/dragonfly/server/world/chunk"
	"github.com/sandertv/gophertunnel/minecraft/nbt"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"golang.org/x/exp/slices"
)

var (
	//go:embed block_states.nbt
	blockStateData []byte
	// blocks holds a list of all registered Blocks indexed by their runtime ID. Blocks that were not explicitly
	// registered are of the type unknownBlock.
	blocks []Block
	// stateRuntimeIDs holds a map for looking up the runtime ID of a block by the stateHash it produces.
	stateRuntimeIDs = map[stateHash]uint32{}
	// nbtBlocks holds a list of NBTer implementations for blocks registered that implement the NBTer interface.
	// These are indexed by their runtime IDs. Blocks that do not implement NBTer have a false value in this slice.
	nbtBlocks []bool
	// randomTickBlocks holds a list of RandomTicker implementations for blocks registered that implement the RandomTicker interface.
	// These are indexed by their runtime IDs. Blocks that do not implement RandomTicker have a false value in this slice.
	randomTickBlocks []bool
	// liquidBlocks holds a list of Liquid implementations for blocks registered that implement the Liquid interface.
	// These are indexed by their runtime IDs. Blocks that do not implement Liquid have a false value in this slice.
	liquidBlocks []bool
	// liquidDisplacingBlocks holds a list of LiquidDisplacer implementations for blocks registered that implement the LiquidDisplacer interface.
	// These are indexed by their runtime IDs. Blocks that do not implement LiquidDisplacer have a false value in this slice.
	liquidDisplacingBlocks []bool
	// airRID is the runtime ID of an air block.
	airRID uint32
)

func AirRID() uint32 {
	return airRID
}

func init() {
	dec := nbt.NewDecoder(bytes.NewBuffer(blockStateData))

	// Register all block states present in the block_states.nbt file. These are all possible options registered
	// blocks may encode to.
	var s blockState
	for {
		if err := dec.Decode(&s); err != nil {
			break
		}
		registerBlockState(s, false)
	}

	chunk.RuntimeIDToState = func(runtimeID uint32) (name string, properties map[string]any, found bool) {
		if runtimeID >= uint32(len(blocks)) {
			return "", nil, false
		}
		name, properties = blocks[runtimeID].EncodeBlock()
		return name, properties, true
	}
	chunk.StateToRuntimeID = func(name string, properties map[string]any) (runtimeID uint32, found bool) {
		rid, ok := stateRuntimeIDs[stateHash{name: name, properties: hashProperties(properties)}]
		return rid, ok
	}
}

func sort_blocks(i, j int) bool {
	nameOne, _ := blocks[i].EncodeBlock()
	nameTwo, _ := blocks[j].EncodeBlock()
	h1 := fnv.New64()
	h1.Write([]byte(nameOne))
	h2 := fnv.New64()
	h2.Write([]byte(nameTwo))
	return nameOne == nameTwo && h1.Sum64() < h2.Sum64()
}

// registerBlockStates inserts multiple blockstates
func registerBlockStates(ss []blockState) {
	var map_rids = map[stateHash]uint32{}

	// add blocks
	for _, s := range ss {
		blocks = append(blocks, unknownBlock{s})
		map_rids[stateHash{s.Name, hashProperties(s.Properties)}] = 0
	}
	// sort the new blocks
	sort.SliceStable(blocks, sort_blocks)

	for id, b := range blocks {
		name, properties := b.EncodeBlock()
		i := stateHash{name: name, properties: hashProperties(properties)}
		rid := uint32(id)
		if name == "minecraft:air" {
			airRID = rid
		}

		// if its one of the added ones
		if _, ok := map_rids[i]; ok {
			if _, ok := stateRuntimeIDs[i]; ok {
				panic(fmt.Sprintf("cannot register the same state twice (%+v)", b))
			}
			map_rids[i] = rid

			nbtBlocks = slices.Insert(nbtBlocks, int(rid), false)
			randomTickBlocks = slices.Insert(randomTickBlocks, int(rid), false)
			liquidBlocks = slices.Insert(liquidBlocks, int(rid), false)
			liquidDisplacingBlocks = slices.Insert(liquidDisplacingBlocks, int(rid), false)
			chunk.FilteringBlocks = slices.Insert(chunk.FilteringBlocks, int(rid), 15)
			chunk.LightBlocks = slices.Insert(chunk.LightBlocks, int(rid), 0)
		}
		stateRuntimeIDs[i] = rid
		hashes.Put(int64(b.Hash()), int64(id))
	}
}

// registerBlockState registers a new blockState to the states slice. The function panics if the properties the
// blockState hold are invalid or if the blockState was already registered.
func registerBlockState(s blockState, order bool) {
	h := stateHash{name: s.Name, properties: hashProperties(s.Properties)}
	if _, ok := stateRuntimeIDs[h]; ok {
		panic(fmt.Sprintf("cannot register the same state twice (%+v)", s))
	}
	rid := uint32(len(blocks))
	if s.Name == "minecraft:air" {
		airRID = rid
	}
	if s.Name == "minecraft:water" {
		chunk.WaterBlocks = append(chunk.WaterBlocks, rid)
	}

	blocks = append(blocks, unknownBlock{s})
	if order {
		sort.SliceStable(blocks, sort_blocks)

		for id, b := range blocks {
			name, properties := b.EncodeBlock()
			i := stateHash{name: name, properties: hashProperties(properties)}
			if name == "minecraft:air" {
				airRID = uint32(id)
			}
			if i == h {
				rid = uint32(id)
			}
			stateRuntimeIDs[i] = uint32(id)
			hashes.Put(int64(b.Hash()), int64(id))
		}
	}
	stateRuntimeIDs[h] = rid

	nbtBlocks = slices.Insert(nbtBlocks, int(rid), false)
	randomTickBlocks = slices.Insert(randomTickBlocks, int(rid), false)
	liquidBlocks = slices.Insert(liquidBlocks, int(rid), false)
	liquidDisplacingBlocks = slices.Insert(liquidDisplacingBlocks, int(rid), false)
	chunk.FilteringBlocks = slices.Insert(chunk.FilteringBlocks, int(rid), 15)
	chunk.LightBlocks = slices.Insert(chunk.LightBlocks, int(rid), 0)
}

func permutate_properties(props map[string]any) []map[string]any {
	var result []map[string]any
	if len(props) == 0 {
		return append(result, map[string]any{})
	}

	f := func(propName string, p1 any) {
		if len(props) == 1 {
			result = append(result, map[string]any{propName: p1})
			return
		}

		delete(props, propName)
		for _, p2 := range permutate_properties(props) {
			res := make(map[string]any)
			res[propName] = p1
			for k, v := range p2 {
				res[k] = v
			}
			result = append(result, res)
		}
	}

	for propName, propVal := range props {
		switch propVal := propVal.(type) {
		case []int32:
			for _, p1 := range propVal {
				f(propName, p1)
			}
		case []any:
			for _, p1 := range propVal {
				f(propName, p1)
			}
		}
	}
	return result
}

func InsertCustomBlocks(entries []protocol.BlockEntry) {
	var states []blockState
	for _, entry := range entries {
		block := ParseBlock(entry)
		for _, props := range permutate_properties(block.Description.Properties) {
			states = append(states, blockState{
				Name:       entry.Name,
				Properties: props,
			})
		}
	}
	registerBlockStates(states)
}

// unknownBlock represents a block that has not yet been implemented. It is used for registering block
// states that haven't yet been added.
type unknownBlock struct {
	blockState
}

// EncodeBlock ...
func (b unknownBlock) EncodeBlock() (string, map[string]any) {
	return b.Name, b.Properties
}

// Model ...
func (unknownBlock) Model() BlockModel {
	return unknownModel{}
}

// Hash ...
func (b unknownBlock) Hash() uint64 {
	return math.MaxUint64
}

func (b unknownBlock) Color() color.RGBA {
	return color.RGBA{255, 0, 255, 255}
}

// blockState holds a combination of a name and properties, together with a version.
type blockState struct {
	Name       string         `nbt:"name"`
	Properties map[string]any `nbt:"states"`
	Version    int32          `nbt:"version"`
}

// stateHash is a struct that may be used as a map key for block states. It contains the name of the block state
// and an encoded version of the properties.
type stateHash struct {
	name, properties string
}

// hashProperties produces a hash for the block properties held by the blockState.
func hashProperties(properties map[string]any) string {
	if properties == nil {
		return ""
	}
	keys := make([]string, 0, len(properties))
	for k := range properties {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return keys[i] < keys[j]
	})

	var b strings.Builder
	for _, k := range keys {
		switch v := properties[k].(type) {
		case bool:
			if v {
				b.WriteByte(1)
			} else {
				b.WriteByte(0)
			}
		case uint8:
			b.WriteByte(v)
		case int32:
			a := *(*[4]byte)(unsafe.Pointer(&v))
			b.Write(a[:])
		case string:
			b.WriteString(v)
		default:
			// If block encoding is broken, we want to find out as soon as possible. This saves a lot of time
			// debugging in-game.
			panic(fmt.Sprintf("invalid block property type %T for property %v", v, k))
		}
	}

	return b.String()
}
