// AI-generated parser
package main

import (
	"errors"
	"strconv"
	"strings"
)

type ByteRange struct {
	start int // inclusive
	end   int // inclusive
}

var (
	ErrInvalidRange       = errors.New("invalid range")
	ErrUnsatisfiableRange = errors.New("unsatisfiable range")
)

func ParseSingleRange(header string, contentLength int) (ByteRange, error) {
	if contentLength <= 0 {
		return ByteRange{}, ErrUnsatisfiableRange
	}

	if !strings.HasPrefix(header, "bytes=") {
		return ByteRange{}, ErrInvalidRange
	}

	s := strings.TrimSpace(strings.TrimPrefix(header, "bytes="))

	left, right, ok := strings.Cut(s, "-")
	if !ok {
		return ByteRange{}, ErrInvalidRange
	}

	left = strings.TrimSpace(left)
	right = strings.TrimSpace(right)

	switch {
	case left == "" && right == "":
		return ByteRange{}, ErrInvalidRange

	case left == "":
		// bytes=-500
		n, err := strconv.Atoi(right)
		if err != nil || n <= 0 {
			return ByteRange{}, ErrInvalidRange
		}

		if n > contentLength {
			n = contentLength
		}

		return ByteRange{
			start: contentLength - n,
			end:   contentLength - 1,
		}, nil

	case right == "":
		// bytes=500-
		start, err := strconv.Atoi(left)
		if err != nil || start < 0 {
			return ByteRange{}, ErrInvalidRange
		}

		if start >= contentLength {
			return ByteRange{}, ErrUnsatisfiableRange
		}

		return ByteRange{
			start: start,
			end:   contentLength - 1,
		}, nil

	default:
		// bytes=500-999
		start, err := strconv.Atoi(left)
		if err != nil || start < 0 {
			return ByteRange{}, ErrInvalidRange
		}

		end, err := strconv.Atoi(right)
		if err != nil || end < start {
			return ByteRange{}, ErrInvalidRange
		}

		if start >= contentLength {
			return ByteRange{}, ErrUnsatisfiableRange
		}

		// end がファイル末尾を超えたら clamp
		if end >= contentLength {
			end = contentLength - 1
		}

		return ByteRange{
			start: start,
			end:   end,
		}, nil
	}
}

const chunkSize = 1 << 20

type ChunkData struct {
	start int
	end   int
}

func CalcChunks(r ByteRange, contentLength int) []ChunkData {
	first := r.start / chunkSize
	last := r.end / chunkSize

	chunks := make([]ChunkData, 0, last-first+1)

	for i := first; i <= last; i++ {
		start := i * chunkSize
		end := start + chunkSize - 1

		if end >= contentLength {
			end = contentLength - 1
		}

		chunks = append(chunks, ChunkData{
			start: start,
			end:   end,
		})
	}

	return chunks
}
