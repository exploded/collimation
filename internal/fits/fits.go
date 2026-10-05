// Package fits reads the primary HDU of 2-D FITS images as written by N.I.N.A.
//
// Only what the collimation pipeline needs is supported: BITPIX 16 (with
// BZERO/BSCALE), 32, -32 and -64, NAXIS = 2, and the ROWORDER keyword.
// Pixels are returned as float32 in display orientation: row 0 is the top of
// the image as N.I.N.A. shows it (x right, y down).
package fits

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
)

const blockSize = 2880

// Header holds the parsed header cards of the primary HDU.
type Header struct {
	cards map[string]string // keyword -> raw value text (strings unquoted)
	order []string
}

// String returns the value of a string keyword, or "" if absent.
func (h Header) String(key string) string { return h.cards[key] }

// Has reports whether the keyword is present.
func (h Header) Has(key string) bool { _, ok := h.cards[key]; return ok }

// Float returns the numeric value of a keyword, or def if absent or not numeric.
func (h Header) Float(key string, def float64) float64 {
	v, ok := h.cards[key]
	if !ok {
		return def
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return def
	}
	return f
}

// Int returns the integer value of a keyword, or def if absent.
func (h Header) Int(key string, def int) int {
	f := h.Float(key, math.NaN())
	if math.IsNaN(f) {
		return def
	}
	return int(math.Round(f))
}

// Keys returns the keywords in file order.
func (h Header) Keys() []string { return h.order }

// Image is a 2-D image with its header.
type Image struct {
	Header Header
	W, H   int
	Pix    []float32 // row-major, len W*H, row 0 = top
}

// At returns the pixel at (x, y).
func (im *Image) At(x, y int) float32 { return im.Pix[y*im.W+x] }

// ReadHeader reads only the header of a FITS file.
func ReadHeader(path string) (Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return Header{}, err
	}
	defer f.Close()
	h, _, err := readHeader(bufio.NewReaderSize(f, 1<<16))
	return h, err
}

// Read reads the header and image data of a FITS file.
func Read(path string) (*Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<20)
	h, _, err := readHeader(r)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	im, err := readData(r, h)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return im, nil
}

func readHeader(r io.Reader) (Header, int, error) {
	h := Header{cards: map[string]string{}}
	block := make([]byte, blockSize)
	n := 0
	for {
		if _, err := io.ReadFull(r, block); err != nil {
			return h, n, fmt.Errorf("reading header: %w", err)
		}
		n++
		for i := 0; i < blockSize; i += 80 {
			card := string(block[i : i+80])
			key := strings.TrimSpace(card[:8])
			if key == "END" {
				if h.String("SIMPLE") != "T" {
					return h, n, errors.New("not a FITS file (SIMPLE != T)")
				}
				return h, n, nil
			}
			if len(card) < 10 || card[8:10] != "= " || key == "" {
				continue
			}
			val := parseValue(card[10:])
			if _, dup := h.cards[key]; !dup {
				h.order = append(h.order, key)
			}
			h.cards[key] = val
		}
		if n > 1000 {
			return h, n, errors.New("header has no END card")
		}
	}
}

// parseValue extracts the value from the value/comment field of a card.
func parseValue(s string) string {
	s = strings.TrimLeft(s, " ")
	if strings.HasPrefix(s, "'") {
		// Quoted string; '' is an escaped quote.
		var b strings.Builder
		for i := 1; i < len(s); i++ {
			if s[i] == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					b.WriteByte('\'')
					i++
					continue
				}
				break
			}
			b.WriteByte(s[i])
		}
		return strings.TrimRight(b.String(), " ")
	}
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

func readData(r io.Reader, h Header) (*Image, error) {
	if h.Int("NAXIS", 0) != 2 {
		return nil, fmt.Errorf("NAXIS = %d, want 2", h.Int("NAXIS", 0))
	}
	w, ht := h.Int("NAXIS1", 0), h.Int("NAXIS2", 0)
	if w <= 0 || ht <= 0 {
		return nil, errors.New("bad image dimensions")
	}
	bitpix := h.Int("BITPIX", 0)
	bzero := h.Float("BZERO", 0)
	bscale := h.Float("BSCALE", 1)
	bpp := abs(bitpix) / 8
	if bpp == 0 {
		return nil, fmt.Errorf("unsupported BITPIX %d", bitpix)
	}
	raw := make([]byte, w*ht*bpp)
	if _, err := io.ReadFull(r, raw); err != nil {
		return nil, fmt.Errorf("reading data: %w", err)
	}
	pix := make([]float32, w*ht)
	be := binary.BigEndian
	switch bitpix {
	case 16:
		for i := range pix {
			pix[i] = float32(float64(int16(be.Uint16(raw[2*i:])))*bscale + bzero)
		}
	case 32:
		for i := range pix {
			pix[i] = float32(float64(int32(be.Uint32(raw[4*i:])))*bscale + bzero)
		}
	case -32:
		for i := range pix {
			pix[i] = float32(float64(math.Float32frombits(be.Uint32(raw[4*i:])))*bscale + bzero)
		}
	case -64:
		for i := range pix {
			pix[i] = float32(math.Float64frombits(be.Uint64(raw[8*i:]))*bscale + bzero)
		}
	default:
		return nil, fmt.Errorf("unsupported BITPIX %d", bitpix)
	}
	im := &Image{Header: h, W: w, H: ht, Pix: pix}
	// The FITS default is bottom-up. N.I.N.A. writes TOP-DOWN when the first
	// stored row is the top of the displayed image.
	if strings.ToUpper(h.String("ROWORDER")) != "TOP-DOWN" {
		flipRows(im)
	}
	return im, nil
}

func flipRows(im *Image) {
	tmp := make([]float32, im.W)
	for y := 0; y < im.H/2; y++ {
		a := im.Pix[y*im.W : (y+1)*im.W]
		b := im.Pix[(im.H-1-y)*im.W : (im.H-y)*im.W]
		copy(tmp, a)
		copy(a, b)
		copy(b, tmp)
	}
}

func abs(i int) int {
	if i < 0 {
		return -i
	}
	return i
}

// Write writes a float32 image as a BITPIX -32 FITS file with the given extra
// header cards (key -> value; strings are quoted automatically). It is used by
// tests and the stub N.I.N.A. server.
func Write(path string, w, h int, pix []float32, extra map[string]any) error {
	var hdr strings.Builder
	card := func(k, v string) {
		c := fmt.Sprintf("%-8s= %20s", k, v)
		hdr.WriteString(fmt.Sprintf("%-80s", c))
	}
	card("SIMPLE", "T")
	card("BITPIX", "-32")
	card("NAXIS", "2")
	card("NAXIS1", strconv.Itoa(w))
	card("NAXIS2", strconv.Itoa(h))
	card("ROWORDER", "'TOP-DOWN'")
	for k, v := range extra {
		switch t := v.(type) {
		case string:
			card(k, "'"+strings.ReplaceAll(t, "'", "''")+"'")
		case int:
			card(k, strconv.Itoa(t))
		case float64:
			card(k, strconv.FormatFloat(t, 'G', 12, 64))
		case bool:
			if t {
				card(k, "T")
			} else {
				card(k, "F")
			}
		}
	}
	hdr.WriteString(fmt.Sprintf("%-80s", "END"))
	s := hdr.String()
	if pad := len(s) % blockSize; pad != 0 {
		s += strings.Repeat(" ", blockSize-pad)
	}
	data := make([]byte, len(pix)*4)
	for i, v := range pix {
		binary.BigEndian.PutUint32(data[4*i:], math.Float32bits(v))
	}
	if pad := len(data) % blockSize; pad != 0 {
		data = append(data, make([]byte, blockSize-pad)...)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(s); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}
