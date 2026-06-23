package loader

import (
	"fmt"
	"os"
)

// resolveRelSource opens a RelSource and returns the parsed RelHeader and a list
// of nodeFile values, each positioned at the first data row. It mirrors
// resolveNodeSource but for relationship files (doc 19 §5.3).
func (l *Loader) resolveRelSource(srcIdx int, src RelSource) (*RelHeader, []nodeFile, error) {
	delim := l.opts.delimiter()
	arrDelim := l.opts.arrayDelim()

	var hdrFields []string
	var dataFiles []nodeFile
	var err error

	if len(src.readers) > 0 {
		if src.Header != "" {
			hdrFields, err = parseHeaderLine(src.Header, delim)
			if err != nil {
				return nil, nil, err
			}
			for i, r := range src.readers {
				dataFiles = append(dataFiles, nodeFile{
					csv:  newCSVReader(r, delim, arrDelim),
					name: fmt.Sprintf("<rel-reader %d>", i+1),
				})
			}
		} else {
			r0csv := newCSVReader(src.readers[0], delim, arrDelim)
			hdrFields, err = nextCSVRecord(r0csv, "<rel-reader 1>")
			if err != nil {
				return nil, nil, err
			}
			dataFiles = append(dataFiles, nodeFile{csv: r0csv, name: "<rel-reader 1>"})
			for i, r := range src.readers[1:] {
				dataFiles = append(dataFiles, nodeFile{
					csv:  newCSVReader(r, delim, arrDelim),
					name: fmt.Sprintf("<rel-reader %d>", i+2),
				})
			}
		}
	} else if len(src.Files) > 0 {
		if src.Header != "" {
			hdrFields, err = parseHeaderLine(src.Header, delim)
			if err != nil {
				return nil, nil, err
			}
			for _, path := range src.Files {
				f, ferr := os.Open(path)
				if ferr != nil {
					return nil, nil, ferr
				}
				dataFiles = append(dataFiles, nodeFile{
					csv:    newCSVReader(f, delim, arrDelim),
					name:   path,
					closer: f,
				})
			}
		} else {
			f0, ferr := os.Open(src.Files[0])
			if ferr != nil {
				return nil, nil, ferr
			}
			r0csv := newCSVReader(f0, delim, arrDelim)
			hdrFields, err = nextCSVRecord(r0csv, src.Files[0])
			if err != nil {
				_ = f0.Close()
				return nil, nil, err
			}
			if len(src.Files) == 1 {
				dataFiles = append(dataFiles, nodeFile{csv: r0csv, name: src.Files[0], closer: f0})
			} else {
				_ = f0.Close()
				for _, path := range src.Files[1:] {
					f, ferr2 := os.Open(path)
					if ferr2 != nil {
						return nil, nil, ferr2
					}
					dataFiles = append(dataFiles, nodeFile{
						csv:    newCSVReader(f, delim, arrDelim),
						name:   path,
						closer: f,
					})
				}
			}
		}
	} else {
		return nil, nil, fmt.Errorf("loader: rel source %d has no files or readers", srcIdx)
	}

	hdr, err := parseRelHeader(hdrFields, src.Type)
	if err != nil {
		return nil, nil, fmt.Errorf("loader: rel source %d header: %w", srcIdx, err)
	}
	return hdr, dataFiles, nil
}
