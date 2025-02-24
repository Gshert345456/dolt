// Copyright 2019-2021 Dolthub, Inc.
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

package nbs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/dolthub/dolt/go/store/hash"
)

// NM4 - rename to objectExistsInChunkSource
func tableExistsInChunkSource(ctx context.Context, s3 *s3ObjectReader, name string, stats *Stats) (bool, error) {
	magic := make([]byte, magicNumberSize)
	n, err := s3.readS3TableFileFromEnd(ctx, name, magic, stats)
	if err != nil {
		return false, err
	}
	if n != len(magic) {
		return false, errors.New("failed to read all data")
	}

	if strings.HasSuffix(name, ArchiveFileSuffix) {
		// dolt magic number is a version byte + DOLTARC. We ignore the version byte here.
		return bytes.Equal(magic[magicNumberSize-doltMagicSize:], []byte(doltMagicNumber)), nil
	} else {
		return bytes.Equal(magic, []byte(magicNumber)), nil
	}
}

func newAWSTableFileChunkSource(ctx context.Context, s3 *s3ObjectReader, al awsLimits, name hash.Hash, chunkCount uint32, q MemoryQuotaProvider, stats *Stats) (cs chunkSource, err error) {
	var tra tableReaderAt
	index, err := loadTableIndex(ctx, stats, chunkCount, q, func(p []byte) error {
		n, err := s3.readS3TableFileFromEnd(ctx, name.String(), p, stats)
		if err != nil {
			return err
		}
		if len(p) != n {
			return errors.New("failed to read all data")
		}
		tra = &s3TableReaderAt{h: name, s3: s3}
		return nil
	})
	if err != nil {
		return &chunkSourceAdapter{}, err
	}

	tr, err := newTableReader(index, tra, s3BlockSize)
	if err != nil {
		_ = index.Close()
		return &chunkSourceAdapter{}, err
	}
	return &chunkSourceAdapter{tr, name}, nil
}

func newAWSArchiveChunkSource(ctx context.Context,
	s3 *s3ObjectReader,
	al awsLimits,
	name string,
	chunkCount uint32,
	q MemoryQuotaProvider,
	stats *Stats) (cs chunkSource, err error) {

	// Perform a readrange of the footer to get the size of the file.
	footer := make([]byte, archiveFooterSize)

	_, sz, err := s3.readRange(ctx, name, footer, httpEndRangeHeader(int(archiveFooterSize)))
	if err != nil {
		return emptyChunkSource{}, err
	}

	rdr := s3ReaderAt{name, s3}

	aRdr, err := newArchiveReader(rdr, sz)
	if err != nil {
		return archiveChunkSource{}, err
	}
	return archiveChunkSource{"panic if we use this", aRdr}, nil
}

func loadTableIndex(ctx context.Context, stats *Stats, cnt uint32, q MemoryQuotaProvider, loadIndexBytes func(p []byte) error) (tableIndex, error) {
	idxSz := int(indexSize(cnt) + footerSize)
	offsetSz := int((cnt - (cnt / 2)) * offsetSize)
	buf, err := q.AcquireQuotaBytes(ctx, idxSz+offsetSz)
	if err != nil {
		return nil, err
	}

	t1 := time.Now()
	if err := loadIndexBytes(buf[:idxSz]); err != nil {
		q.ReleaseQuotaBytes(len(buf))
		return nil, err
	}
	stats.IndexReadLatency.SampleTimeSince(t1)
	stats.IndexBytesPerRead.Sample(uint64(len(buf)))

	idx, err := parseTableIndexWithOffsetBuff(buf[:idxSz], buf[idxSz:], q)
	if err != nil {
		q.ReleaseQuotaBytes(len(buf))
	}
	return idx, err
}

// NM4 - See if we can get rid of this.
type s3ReaderAt struct {
	name string
	rdr  *s3ObjectReader
}

func (s s3ReaderAt) ReadAt(p []byte, off int64) (n int, err error) {
	// NM4 - need a read context. Stats maybe not?
	return s.rdr.ReadAt(context.Background(), s.name, p, off, &Stats{})
}

var _ io.ReaderAt = &s3ReaderAt{}
