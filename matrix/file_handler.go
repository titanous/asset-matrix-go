package matrix

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"sort"
)

type FileHandler struct {
	HandlerChain   []Handler
	FileSet        []*FileHandler
	ParentHandlers []*FileHandler
	File           *File
}

func NewFileHandler(inExt string) *FileHandler {
	fileHandler := new(FileHandler)
	fileHandler.buildHandlerChain(inExt)
	return fileHandler
}

func (fileHandler *FileHandler) buildHandlerChain(inExt string) {
	handlers := FindHandlers(inExt)
	if handlers == nil && len(fileHandler.HandlerChain) == 0 {
		fileHandler.HandlerChain = append(fileHandler.HandlerChain, NewDefaultHandler(inExt))
		return
	}

	canAppendFlow := true
	for outExt, rh := range handlers {
		if rh.Options.InputMode == InputModeFlow && canAppendFlow {
			canAppendFlow = false
			fileHandler.HandlerChain = append(fileHandler.HandlerChain, rh.Handler)
			fileHandler.buildHandlerChain(outExt)
		} else {
			fh := NewForkHandler(NewFileHandler(inExt), inExt)
			fileHandler.HandlerChain = append(fileHandler.HandlerChain, fh)
		}
	}
}

func (fileHandler *FileHandler) concatenationOrderLess(a *ConcatenationHandler, b *ConcatenationHandler) bool {
	ai, bi := -1, -1
	for i, fh := range fileHandler.FileSet {
		if a.child == fh {
			ai = i
		}

		if b.child == fh {
			bi = i
		}

		if ai > -1 && bi > -1 {
			break
		}
	}

	return ai < bi
}

func (fileHandler *FileHandler) addHandlerAfterIndex(handler Handler, index int) {
	l := len(fileHandler.HandlerChain)
	chain := make([]Handler, 0, l)
	for i, h := range fileHandler.HandlerChain {
		chain = append(chain, h)
		if i == index {
			if i < l-1 {
				if ch, ok := fileHandler.HandlerChain[i+1].(*ConcatenationHandler); ok {
					if chandler, ok := handler.(*ConcatenationHandler); ok {
						if fileHandler.concatenationOrderLess(chandler, ch) {
							index++
							continue
						}
					}
				}
			}
			chain = append(chain, handler)
		}
	}
	fileHandler.HandlerChain = chain
}

func (fileHandler *FileHandler) AddFileHandler(fh *FileHandler) {
	if fh != fileHandler {
		fh.AddParentFileHandler(fileHandler)
	}
	fileHandler.FileSet = append(fileHandler.FileSet, fh)
}

func (fileHandler *FileHandler) AddParentFileHandler(fh *FileHandler) {
	fileHandler.ParentHandlers = append(fileHandler.ParentHandlers, fh)
}

func (fileHandler *FileHandler) CleanConcatenationChain() {
	var children []*FileHandler
	var chain []Handler
	var chandlers []*ConcatenationHandler
	for _, h := range fileHandler.HandlerChain {
		uniq := true
		for _, handler := range chain {
			if ch, ok := h.(*ConcatenationHandler); ok {
				if chandler, ok := handler.(*ConcatenationHandler); ok {
					if ch.child == chandler.child {
						uniq = false
						break
					}
				}
			}
		}
		if uniq {
			chain = append(chain, h)
		} else {
			continue
		}

		if ch, ok := h.(*ConcatenationHandler); ok {
			children = append(children, ch.child)
			chandlers = append(chandlers, ch)
		}
	}

	fileHandler.HandlerChain = chain

	for _, ch := range chandlers {
		for _, fh := range children {
			ch.child.removeChild(fh)
		}
	}
}

func (fileHandler *FileHandler) removeChild(fh *FileHandler) {
	var chain []Handler
	for _, h := range fileHandler.HandlerChain {
		if ch, ok := h.(*ConcatenationHandler); ok {
			if ch.child == fh {
				continue
			} else {
				ch.child.removeChild(fh)
			}
		}

		chain = append(chain, h)
	}

	fileHandler.HandlerChain = chain
}

func (parent *FileHandler) concatenateAtIndex(child *FileHandler, handlerIndex int) {
	mode := ConcatenationModePrepend
	for _, fh := range parent.FileSet {
		if fh == child {
			break
		}

		if fh == parent {
			mode = ConcatenationModeAppend
			break
		}
	}

	ext := parent.HandlerChain[handlerIndex].OutputExt()
	parent.addHandlerAfterIndex(NewConcatenationHandler(parent, child, mode, ext), handlerIndex)
}

func (fileHandler *FileHandler) MergeWithParents() error {
	// sort parent handlers by lowest len(fh.HandlerChain)
	sort.Sort(byLenHandlerChain(fileHandler.ParentHandlers))
	for _, parent := range fileHandler.ParentHandlers {
		// ensure the last handler in each chain have the same out ext
		index, err := removeIncompatibleHandlers(fileHandler.HandlerChain, parent.HandlerChain)
		if err != nil {
			return err
		}

		// add concatenation handler to parent
		parent.concatenateAtIndex(fileHandler, index)
	}

	return nil
}

func removeIncompatibleHandlers(a []Handler, b []Handler) (index int, err error) {
	findIndex := func() int {
		for i := len(a) - 1; i >= 0; i-- {
			for j := len(b) - 1; j >= 0; j-- {
				if _, ok := b[j].(*ConcatenationHandler); ok {
					if j > 0 {
						continue
					}
				}
				if a[i].OutputExt() == b[j].OutputExt() {
					a = a[0:i]
					return j
				}
			}
		}
		return -1
	}
	index = findIndex()
	if index == -1 {
		err = fmt.Errorf("matrix: FileHandler: incompatible handler chains: %v, %v", a, b)
	}
	return
}

func (fileHandler *FileHandler) Handle(out io.Writer, name *string, exts *[]string) (err error) {
	if !shouldOpenFD(1) {
		waitFD(1)
	}

	f, err := os.Open(fileHandler.File.Path())
	if err != nil {
		return
	}

	r, w := io.Pipe()

	buf := new(bytes.Buffer)
	go func() {
		_, err = io.Copy(buf, f)

		if err != nil {
			w.CloseWithError(err)
		}

		f.Close()
		fdClosed(1)
	}()

	go func() {
		_, err = io.Copy(w, bytes.NewBuffer(buf.Bytes()[fileHandler.File.dataByteOffset:]))

		w.CloseWithError(err)
	}()

	handlerFn := func(handler Handler, in io.Reader) *io.PipeReader {
		r, w := io.Pipe()
		go func() {
			if fdHandler, ok := handler.(FDHandler); ok {
				// handler requires file descriptors
				nFds := fdHandler.RequiredFds()

				if nFds > 0 && !shouldOpenFD(nFds) {
					data := new(bytes.Buffer)

					_, err := io.Copy(data, in)
					if err != nil {
						w.CloseWithError(err)
						return
					}

					in = data

					waitFD(nFds)
				}
				defer fdClosed(nFds)
			}

			err = handler.Handle(in, w, name, exts)
			w.CloseWithError(err)
		}()
		return r
	}

	for _, handler := range fileHandler.HandlerChain {
		r = handlerFn(handler, r)
	}

	_, err = io.Copy(out, r)
	return
}
