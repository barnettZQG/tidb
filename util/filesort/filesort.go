// Copyright 2017 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package filesort

import (
	"container/heap"
	"encoding/binary"
	"io"
	"os"
	"path"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/juju/errors"
	"github.com/pingcap/tidb/sessionctx/variable"
	"github.com/pingcap/tidb/util/codec"
	"github.com/pingcap/tidb/util/types"
)

var nWorkers = 10

var gSc *variable.StatementContext
var gByDesc []bool

func lessThan(sc *variable.StatementContext, i []types.Datum, j []types.Datum, byDesc []bool) (bool, error) {
	for k := range byDesc {
		v1 := i[k]
		v2 := j[k]

		ret, err := v1.CompareDatum(sc, v2)
		if err != nil {
			return false, errors.Trace(err)
		}

		if byDesc[k] {
			ret = -ret
		}

		if ret < 0 {
			return true, nil
		} else if ret > 0 {
			return false, nil
		}
	}
	return false, nil
}

type comparableRow struct {
	key    []types.Datum
	val    []types.Datum
	handle int64
}

type item struct {
	index int // source file index
	value *comparableRow
}

// Min-heap of comparableRows
type rowHeap struct {
	ims []*item
	err error
}

// Len implements heap.Interface Len interface.
func (rh *rowHeap) Len() int { return len(rh.ims) }

// Swap implements heap.Interface Swap interface.
func (rh *rowHeap) Swap(i, j int) { rh.ims[i], rh.ims[j] = rh.ims[j], rh.ims[i] }

// Less implements heap.Interface Less interface.
func (rh *rowHeap) Less(i, j int) bool {
	l := rh.ims[i].value.key
	r := rh.ims[j].value.key
	ret, err := lessThan(gSc, l, r, gByDesc)
	if rh.err == nil {
		rh.err = err
	}
	return ret
}

// Push implements heap.Interface Push interface.
func (rh *rowHeap) Push(x interface{}) {
	rh.ims = append(rh.ims, x.(*item))
}

// Push implements heap.Interface Pop interface.
func (rh *rowHeap) Pop() interface{} {
	old := rh.ims
	n := len(old)
	x := old[n-1]
	rh.ims = old[0 : n-1]
	return x
}

// FileSorter sorts the given rows according to the byDesc order.
// FileSorter can sort rows that exceed predefined memory capacity.
type FileSorter struct {
	workers  []*Worker
	cWorker  int
	nWorkers int

	mu      sync.Mutex
	wg      sync.WaitGroup
	tmpDir  string
	files   []string
	nFiles  int
	closed  bool
	fetched bool

	rowHeap    *rowHeap
	fds        []*os.File
	rowBytes   []byte
	head       []byte
	dcod       []types.Datum
	keySize    int
	valSize    int
	maxRowSize int
}

func (fs *FileSorter) getUniqueFileName() string {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	ret := path.Join(fs.tmpDir, strconv.Itoa(fs.nFiles))
	fs.nFiles++
	return ret
}

func (fs *FileSorter) appendFileName(fn string) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.files = append(fs.files, fn)
}

func (fs *FileSorter) closeAllFiles() error {
	for _, fd := range fs.fds {
		err := fd.Close()
		if err != nil {
			return errors.Trace(err)
		}
	}
	err := os.RemoveAll(fs.tmpDir)
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}

// Perform external file sort.
func (fs *FileSorter) externalSort() (*comparableRow, error) {
	if !fs.fetched {
		for _, w := range fs.workers {
			if !w.busy && len(w.buf) > 0 {
				fs.wg.Add(1)
				go w.flushToFile()
			}
		}

		fs.wg.Wait()

		for _, w := range fs.workers {
			if w.err != nil {
				return nil, errors.Trace(w.err)
			}
			if w.rowSize > fs.maxRowSize {
				fs.maxRowSize = w.rowSize
			}
		}

		heap.Init(fs.rowHeap)
		if fs.rowHeap.err != nil {
			return nil, errors.Trace(fs.rowHeap.err)
		}

		fs.rowBytes = make([]byte, fs.maxRowSize)

		err := fs.openAllFiles()
		if err != nil {
			return nil, errors.Trace(err)
		}

		for id := range fs.fds {
			row, err := fs.fetchNextRow(id)
			if err != nil {
				return nil, errors.Trace(err)
			}
			if row == nil {
				return nil, errors.New("file is empty")
			}

			im := &item{
				index: id,
				value: row,
			}

			heap.Push(fs.rowHeap, im)
			if fs.rowHeap.err != nil {
				return nil, errors.Trace(fs.rowHeap.err)
			}
		}

		fs.fetched = true
	}

	if fs.rowHeap.Len() > 0 {
		im := heap.Pop(fs.rowHeap).(*item)
		if fs.rowHeap.err != nil {
			return nil, errors.Trace(fs.rowHeap.err)
		}

		row, err := fs.fetchNextRow(im.index)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if row != nil {
			im := &item{
				index: im.index,
				value: row,
			}

			heap.Push(fs.rowHeap, im)
			if fs.rowHeap.err != nil {
				return nil, errors.Trace(fs.rowHeap.err)
			}
		}

		return im.value, nil
	}

	return nil, nil
}

func (fs *FileSorter) openAllFiles() error {
	for _, fname := range fs.files {
		fd, err := os.Open(fname)
		if err != nil {
			return errors.Trace(err)
		}
		fs.fds = append(fs.fds, fd)
	}
	return nil
}

// Fetch the next row given the source file index.
func (fs *FileSorter) fetchNextRow(index int) (*comparableRow, error) {
	var (
		err error
		n   int
	)
	n, err = fs.fds[index].Read(fs.head)
	if err == io.EOF {
		return nil, nil
	}
	if err != nil {
		return nil, errors.Trace(err)
	}
	if n != 8 {
		return nil, errors.New("incorrect header")
	}
	rowSize := int(binary.BigEndian.Uint64(fs.head))

	n, err = fs.fds[index].Read(fs.rowBytes)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if n != rowSize {
		return nil, errors.New("incorrect row")
	}

	fs.dcod, err = codec.Decode(fs.rowBytes, fs.keySize+fs.valSize+1)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return &comparableRow{
		key:    fs.dcod[:fs.keySize],
		val:    fs.dcod[fs.keySize : fs.keySize+fs.valSize],
		handle: fs.dcod[fs.keySize+fs.valSize:][0].GetInt64(),
	}, nil
}

// Input adds one row into FileSorter.
// Caller should not call Input after calling Output.
func (fs *FileSorter) Input(key []types.Datum, val []types.Datum, handle int64) error {
	if fs.closed {
		return errors.New("FileSorter has been closed")
	}
	if fs.fetched {
		return errors.New("call input after output")
	}

	assigned := false
	row := &comparableRow{
		key:    key,
		val:    val,
		handle: handle,
	}

	for {
		for i := 0; i < fs.nWorkers; i++ {
			wid := (fs.cWorker + i) % fs.nWorkers
			if !fs.workers[wid].busy {
				err := fs.workers[wid].input(row)
				if err != nil {
					return errors.Trace(err)
				}
				assigned = true
				fs.cWorker = wid
				break
			}
		}
		if assigned {
			break
		} else {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return nil
}

// Output gets the next sorted row.
func (fs *FileSorter) Output() ([]types.Datum, []types.Datum, int64, error) {
	if fs.closed {
		return nil, nil, 0, errors.New("FileSorter has been closed")
	}
	r, err := fs.externalSort()
	if err != nil {
		return nil, nil, 0, errors.Trace(err)
	} else if r != nil {
		return r.key, r.val, r.handle, nil
	} else {
		return nil, nil, 0, nil
	}
}

// Close terminates the input or output process and discards all remaining data.
func (fs *FileSorter) Close() error {
	if fs.closed {
		return errors.New("FileSorter has been closed")
	}
	fs.wg.Wait()
	err := fs.closeAllFiles()
	if err != nil {
		return errors.Trace(err)
	}
	for _, w := range fs.workers {
		w.buf = w.buf[:0]
	}
	fs.closed = true
	return nil
}

// Worker actually sorts the file.
type Worker struct {
	ctx     *FileSorter
	busy    bool
	keySize int
	valSize int
	rowSize int
	bufSize int
	buf     []*comparableRow
	head    []byte
	err     error
}

func (w *Worker) Len() int { return len(w.buf) }

func (w *Worker) Swap(i, j int) { w.buf[i], w.buf[j] = w.buf[j], w.buf[i] }

func (w *Worker) Less(i, j int) bool {
	l := w.buf[i].key
	r := w.buf[j].key
	// TODO: handle error here
	ret, _ := lessThan(gSc, l, r, gByDesc)
	return ret
}

func (w *Worker) input(row *comparableRow) error {
	w.buf = append(w.buf, row)

	if len(w.buf) >= w.bufSize {
		w.busy = true
		w.ctx.wg.Add(1)
		go w.flushToFile()
	}

	if w.err != nil {
		return errors.Trace(w.err)
	}
	return nil
}

// Flush the buffer to file if it is full.
func (w *Worker) flushToFile() {
	defer w.ctx.wg.Done()
	var (
		err        error
		outputFile *os.File
		outputByte []byte
		prevLen    int
	)

	sort.Sort(w)
	if w.err != nil {
		return
	}

	fileName := w.ctx.getUniqueFileName()

	outputFile, err = os.OpenFile(fileName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		w.err = err
		return
	}
	defer outputFile.Close()

	for _, row := range w.buf {
		prevLen = len(outputByte)
		outputByte = append(outputByte, w.head...)
		outputByte, err = codec.EncodeKey(outputByte, row.key...)
		if err != nil {
			w.err = err
			return
		}
		outputByte, err = codec.EncodeKey(outputByte, row.val...)
		if err != nil {
			w.err = err
			return
		}
		outputByte, err = codec.EncodeKey(outputByte, types.NewIntDatum(row.handle))
		if err != nil {
			w.err = err
			return
		}

		if len(outputByte)-prevLen-8 > w.rowSize {
			w.rowSize = len(outputByte) - prevLen - 8
		}
		binary.BigEndian.PutUint64(w.head, uint64(len(outputByte)-prevLen-8))
		for i := 0; i < 8; i++ {
			outputByte[prevLen+i] = w.head[i]
		}
	}

	_, err = outputFile.Write(outputByte)
	if err != nil {
		w.err = err
		return
	}

	w.ctx.appendFileName(fileName)
	w.buf = w.buf[:0]
	w.busy = false
	return
}

// Builder builds a new FileSorter.
type Builder struct {
	keySize int
	valSize int
	bufSize int
	tmpDir  string
}

// SetSC sets StatementContext instance which is required in row comparison.
func (b *Builder) SetSC(sc *variable.StatementContext) *Builder {
	gSc = sc
	return b
}

// SetSchema sets the schema of row, including key size and value size.
func (b *Builder) SetSchema(keySize, valSize int) *Builder {
	b.keySize = keySize
	b.valSize = valSize
	return b
}

// SetBuf sets the number of rows FileSorter can hold in memory at a time.
func (b *Builder) SetBuf(bufSize int) *Builder {
	b.bufSize = bufSize
	return b
}

// SetDesc sets the ordering rule of row comparison.
func (b *Builder) SetDesc(byDesc []bool) *Builder {
	gByDesc = byDesc
	return b
}

// SetDir sets the working directory for FileSorter.
func (b *Builder) SetDir(tmpDir string) *Builder {
	b.tmpDir = tmpDir
	return b
}

// Build creates a FileSorter instance using given data.
func (b *Builder) Build() (*FileSorter, error) {
	// Sanity checks
	if gSc == nil {
		return nil, errors.New("StatementContext is nil")
	}
	if b.keySize != len(gByDesc) {
		return nil, errors.New("mismatch in key size and byDesc slice")
	}
	if b.keySize <= 0 {
		return nil, errors.New("key size is not positive")
	}
	if b.valSize <= 0 {
		return nil, errors.New("value size is not positive")
	}
	if b.bufSize <= 0 {
		return nil, errors.New("buffer size is not positive")
	}
	_, err := os.Stat(b.tmpDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, errors.New("tmpDir does not exist")
		}
		return nil, errors.Trace(err)
	}

	ws := make([]*Worker, nWorkers)
	for i := range ws {
		ws[i] = &Worker{
			keySize: b.keySize,
			valSize: b.valSize,
			rowSize: b.keySize + b.valSize + 1,
			bufSize: b.bufSize / nWorkers,
			buf:     make([]*comparableRow, 0, b.bufSize/nWorkers),
			head:    make([]byte, 8),
		}
	}

	rh := &rowHeap{
		ims: make([]*item, 0),
	}

	fs := &FileSorter{
		workers:  ws,
		cWorker:  0,
		nWorkers: nWorkers,

		head:    make([]byte, 8),
		dcod:    make([]types.Datum, 0, b.keySize+b.valSize+1),
		keySize: b.keySize,
		valSize: b.valSize,

		tmpDir:  b.tmpDir,
		files:   make([]string, 0),
		rowHeap: rh,
	}

	for i := 0; i < nWorkers; i++ {
		fs.workers[i].ctx = fs
	}

	return fs, nil
}
