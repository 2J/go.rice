package main

import (
	"bufio"
	"bytes"
	"fmt"
	"go/build"
	"go/format"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/valyala/fasttemplate"
)

const boxFilename = "rice-box.go"

const lowerhex = "0123456789abcdef"

func writeBoxesGo(pkg *build.Package, out io.Writer) error {
	boxMap := findBoxes(pkg)

	// notify user when no calls to rice.FindBox are made (is this an error and therefore os.Exit(1) ?
	if len(boxMap) == 0 {
		fmt.Println("no calls to rice.FindBox() found")
		return nil
	}

	verbosef("\n")

	var boxes []*boxDataType

	for boxname := range boxMap {
		// find path and filename for this box
		boxPath := filepath.Join(pkg.Dir, boxname)

		// Check to see if the path for the box is a symbolic link.  If so, simply
		// box what the symbolic link points to.  Note: the filepath.Walk function
		// will NOT follow any nested symbolic links.  This only handles the case
		// where the root of the box is a symbolic link.
		symPath, serr := os.Readlink(boxPath)
		if serr == nil {
			boxPath = symPath
		}

		// verbose info
		verbosef("embedding box '%s' to '%s'\n", boxname, boxFilename)

		// read box metadata
		boxInfo, ierr := os.Stat(boxPath)
		if ierr != nil {
			return fmt.Errorf("Error: unable to access box at %s\n", boxPath)
		}

		// create box datastructure (used by template)
		box := &boxDataType{
			BoxName: boxname,
			UnixNow: boxInfo.ModTime().Unix(),
			Files:   make([]*fileDataType, 0),
			Dirs:    make(map[string]*dirDataType),
		}

		if !boxInfo.IsDir() {
			return fmt.Errorf("Error: Box %s must point to a directory but points to %s instead\n",
				boxname, boxPath)
		}

		// fill box datastructure with file data
		err := filepath.Walk(boxPath, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return fmt.Errorf("error walking box: %s\n", err)
			}

			filename := strings.TrimPrefix(path, boxPath)
			filename = strings.Replace(filename, "\\", "/", -1)
			filename = strings.TrimPrefix(filename, "/")
			if info.IsDir() {
				dirData := &dirDataType{
					Identifier: "dir" + nextIdentifier(),
					FileName:   filename,
					ModTime:    info.ModTime().Unix(),
					ChildFiles: make([]*fileDataType, 0),
					ChildDirs:  make([]*dirDataType, 0),
				}
				verbosef("\tincludes dir: '%s'\n", dirData.FileName)
				box.Dirs[dirData.FileName] = dirData

				// add tree entry (skip for root, it'll create a recursion)
				if dirData.FileName != "" {
					pathParts := strings.Split(dirData.FileName, "/")
					parentDir := box.Dirs[strings.Join(pathParts[:len(pathParts)-1], "/")]
					parentDir.ChildDirs = append(parentDir.ChildDirs, dirData)
				}
			} else {
				fileData := &fileDataType{
					Identifier: "file" + nextIdentifier(),
					FileName:   filename,
					ModTime:    info.ModTime().Unix(),
				}
				verbosef("\tincludes file: '%s'\n", fileData.FileName)
				/*
					fileData.Content, err = ioutil.ReadFile(path)
					if err != nil {
						return fmt.Errorf("error reading file content while walking box: %s\n", err)
					}
				*/
				fileData.Content = []byte("{%" + path + "%}")
				box.Files = append(box.Files, fileData)

				// add tree entry
				pathParts := strings.Split(fileData.FileName, "/")
				parentDir := box.Dirs[strings.Join(pathParts[:len(pathParts)-1], "/")]
				if parentDir == nil {
					return fmt.Errorf("Error: parent of %s is not within the box\n", path)
				}
				parentDir.ChildFiles = append(parentDir.ChildFiles, fileData)
			}
			return nil
		})
		if err != nil {
			return err
		}
		boxes = append(boxes, box)

	}

	embedSourceUnformated := bytes.NewBuffer(make([]byte, 0))

	// execute template to buffer
	err := tmplEmbeddedBox.Execute(
		embedSourceUnformated,
		embedFileDataType{pkg.Name, boxes},
	)
	if err != nil {
		return fmt.Errorf("error writing embedded box to file (template execute): %s\n", err)
	}

	// format the source code
	embedSource, err := format.Source(embedSourceUnformated.Bytes())
	if err != nil {
		return fmt.Errorf("error formatting embedSource: %s\n", err)
	}

	// write source to file
	// inject file contents
	ft, err := fasttemplate.NewTemplate(string(embedSource), "{%", "%}")
	if err != nil {
		return fmt.Errorf("error writing embedSource to file (fasttemplate compile): %s\n", err)
	}

	bufWriter := bufio.NewWriterSize(out, 100*1024)
	bufReader := bufio.NewReaderSize(nil, 100*1024)

	/**/
	_, err = ft.ExecuteFunc(bufWriter, func(w io.Writer, tag string) (int, error) {
		fileName, err := strconv.Unquote("\"" + tag + "\"")
		if err != nil {
			return 0, err
		}
		f, err := os.Open(fileName)
		if err != nil {
			return 0, err
		}

		bufReader.Reset(f)
		n := 0

		for {
			data, peekErr := bufReader.Peek(utf8.UTFMax)
			// even if peekErr is io.EOF, we need to process data
			if peekErr != nil && peekErr != io.EOF {
				err = peekErr
				break
			}
			// break if done
			if len(data) == 0 {
				break
			}
			var discard, n2 int
			r, width := utf8.DecodeRune(data)
			if width == 1 && r == utf8.RuneError {
				w.Write([]byte{'\\', 'x', lowerhex[data[0]>>4], lowerhex[data[0]&0xF]})
				n2 = 4
				discard = 1
			} else {
				discard = width
				if r == rune('"') || r == '\\' { // always backslashed
					w.Write([]byte{'\\', byte(r)})
					n2 = 2
				} else if strconv.IsPrint(r) {
					w.Write(data[:width])
					n2 = width
				} else {
					switch r {
					case '\a':
						w.Write([]byte{'\\', 'a'})
						n2 = 2
					case '\b':
						w.Write([]byte{'\\', 'b'})
						n2 = 2
					case '\f':
						w.Write([]byte{'\\', 'f'})
						n2 = 2
					case '\n':
						w.Write([]byte{'\\', 'n'})
						n2 = 2
					case '\r':
						w.Write([]byte{'\\', 'r'})
						n2 = 2
					case '\t':
						w.Write([]byte{'\\', 't'})
						n2 = 2
					case '\v':
						w.Write([]byte{'\\', 'v'})
						n2 = 2
					default:
						switch {
						case r < ' ':
							w.Write([]byte{'\\', 'x', lowerhex[data[0]>>4], lowerhex[data[0]&0xF]})
							n2 = 4
						case r > utf8.MaxRune:
							r = 0xFFFD
							fallthrough
						case r < 0x10000:
							w.Write([]byte{'\\', 'u'})
							n2 = 2
							for s := 12; s >= 0; s -= 4 {
								w.Write([]byte{lowerhex[r>>uint(s)&0xF]})
								n2++
							}
						default:
							w.Write([]byte{'\\', 'U'})
							n2 = 2
							for s := 28; s >= 0; s -= 4 {
								w.Write([]byte{lowerhex[r>>uint(s)&0xF]})
								n2++
							}
						}
					}
				}
			}
			bufReader.Discard(discard)
			n += n2
		}

		//	for {
		//		var n2 int
		//		data, err2 := bufReader.Peek(utf8.UTFMax)
		//		if err2 == io.EOF {
		//			break
		//		}
		//		if err2 != nil {
		//			err = err2
		//			break
		//		}
		//		discard := 1
		//		switch b := data[0]; b {
		//		case '\\':
		//			n2, err2 = w.Write([]byte(`\\`))
		//		case '"':
		//			n2, err2 = w.Write([]byte(`\"`))
		//		case '\n':
		//			n2, err2 = w.Write([]byte(`\n`))

		//		case '\x00':
		//			// https://golang.org/ref/spec#Source_code_representation: "Implementation
		//			// restriction: For compatibility with other tools, a compiler may
		//			// disallow the NUL character (U+0000) in the source text."
		//			n2, err2 = w.Write([]byte(`\x00`))

		//		default:
		//			// https://golang.org/ref/spec#Source_code_representation: "Implementation
		//			// restriction: […] A byte order mark may be disallowed anywhere else in
		//			// the source."
		//			const byteOrderMark = '\uFEFF'

		//			if r, size := utf8.DecodeRune(data); r != utf8.RuneError && r != byteOrderMark {
		//				n2, err2 = w.Write(data[:size])
		//				discard = size
		//			} else {
		//				n2, err2 = fmt.Fprintf(w, `\x%02x`, b)
		//			}
		//		}
		//		n += n2
		//		bufReader.Discard(discard)
		//		if err2 != nil {
		//			err = err2
		//			break
		//		}
		//	}

		//	for {
		//		r, size, err2 := bufReader.ReadRune()
		//		if err2 == io.EOF {
		//			err = nil
		//			break
		//		}
		//		if err2 != nil {
		//			err = err2
		//			break
		//		}
		//		var n2 int
		//		if r == unicode.ReplacementChar && size == 1 {
		//			bufReader.UnreadByte()
		//			b, err2 := bufReader.ReadByte()
		//			if err2 != nil {
		//				err = err2
		//				break
		//			}
		//			n2, err2 = fmt.Fprintf(w, "\\x%x", b)
		//		} else {
		//			if r == '"' {
		//				n2, err2 = fmt.Fprint(w, "\\\"")
		//			} else if r == '\'' {
		//				n2, err2 = fmt.Fprint(w, "'")
		//			} else {
		//				quoted := strconv.QuoteRune(r)
		//				n2, err2 = fmt.Fprintf(w, "%v", quoted[1:len(quoted)-1])
		//			}
		//		}
		//		n += n2
		//		if err2 != nil {
		//			err = err2
		//			break
		//		}
		//	}

		f.Close()

		return int(n), err
	})
	/**/

	/*
		_, err = ft.ExecuteFunc(bufWriter, func(w io.Writer, tag string) (int, error) {
			fileContent, err := ioutil.ReadFile(tag)
			if err != nil {
				return 0, err
			}
			quoted := strconv.Quote(string(fileContent))
			return fmt.Fprint(w, quoted[1:len(quoted)-1])
		})
	*/
	if err != nil {
		return fmt.Errorf("error writing embedSource to file: %s\n", err)
	}
	err = bufWriter.Flush()
	if err != nil {
		return fmt.Errorf("error writing embedSource to file: %s\n", err)
	}
	return nil
}

func operationEmbedGo(pkg *build.Package) {
	// create go file for box
	boxFile, err := os.Create(filepath.Join(pkg.Dir, boxFilename))
	if err != nil {
		log.Printf("error creating embedded box file: %s\n", err)
		os.Exit(1)
	}
	defer boxFile.Close()

	err = writeBoxesGo(pkg, boxFile)
	if err != nil {
		log.Printf("error creating embedded box file: %s\n", err)
		os.Exit(1)
	}
}
