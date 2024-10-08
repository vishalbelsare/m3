// Copyright (c) 2019 Uber Technologies, Inc.
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

package index

import (
	"github.com/cespare/xxhash/v2"

	"github.com/m3db/m3/src/x/ident"
)

const (
	defaultInitialAggregatedValuesMapSize = 10
)

// NewAggregateValuesMap builds an AggregateValuesMap, which is primarily used
// for checking for existence of particular ident.IDs.
func NewAggregateValuesMap(idPool ident.Pool) *AggregateValuesMap {
	return _AggregateValuesMapAlloc(_AggregateValuesMapOptions{
		hash: func(k ident.ID) AggregateValuesMapHash {
			return AggregateValuesMapHash(xxhash.Sum64(k.Bytes()))
		},
		equals: func(x, y ident.ID) bool {
			return x.Equal(y)
		},
		copy: func(k ident.ID) ident.ID {
			return idPool.Clone(k)
		},
		finalize: func(k ident.ID) {
			k.Finalize()
		},
		initialSize: defaultInitialAggregatedValuesMapSize,
	})
}
