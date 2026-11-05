package cut

import (
	"context"
	"io"
)

// blockCache answers reads from blocks of the recording it has read lately, so
// that the search, which reads near the same places again and again, asks the
// DVR once for each.
type blockCache struct {
	src    Source
	blocks map[int64][]byte // by block number
	order  []int64          // the blocks held, oldest first
}

const (
	blockSize = 32 << 10
	maxBlocks = 256 // 8 MB
)

func newBlockCache(src Source) *blockCache {
	return &blockCache{src: src, blocks: map[int64][]byte{}}
}

func (c *blockCache) Size() int64 { return c.src.Size() }

func (c *blockCache) ReadAt(ctx context.Context, off, n int64) ([]byte, error) {
	size := c.src.Size()
	end := min(off+n, size)
	if off < 0 || off >= end {
		return nil, io.EOF
	}
	first, last := off/blockSize, (end-1)/blockSize
	missingFrom, missingTo := int64(-1), int64(-1)
	for b := first; b <= last; b++ {
		if _, ok := c.blocks[b]; !ok {
			if missingFrom < 0 {
				missingFrom = b
			}
			missingTo = b
		}
	}
	if missingFrom >= 0 {
		// One request for the run of blocks that are missing.
		start, stop := missingFrom*blockSize, min((missingTo+1)*blockSize, size)
		data, err := c.src.ReadAt(ctx, start, stop-start)
		if err != nil {
			return nil, err
		}
		for b := missingFrom; b <= missingTo; b++ {
			lo := (b - missingFrom) * blockSize
			if lo >= int64(len(data)) {
				break
			}
			c.put(b, data[lo:min(lo+blockSize, int64(len(data)))])
		}
	}
	var out []byte
	for b := first; b <= last; b++ {
		blk, ok := c.blocks[b]
		if !ok {
			break // the DVR gave less than was asked for
		}
		lo := max(off-b*blockSize, 0)
		hi := min(end-b*blockSize, int64(len(blk)))
		if lo >= hi {
			break
		}
		if first == last {
			return blk[lo:hi], nil
		}
		out = append(out, blk[lo:hi]...)
	}
	if len(out) == 0 {
		return nil, io.EOF
	}
	return out, nil
}

func (c *blockCache) put(b int64, data []byte) {
	if _, ok := c.blocks[b]; !ok {
		c.order = append(c.order, b)
		for len(c.order) > maxBlocks {
			delete(c.blocks, c.order[0])
			c.order = c.order[1:]
		}
	}
	c.blocks[b] = data
}
