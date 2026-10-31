package jpegcheck

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// verdict は jpeg.Decode と Validate の結果を、比べられる形にしたもの。
type verdict struct {
	ok     bool
	err    string
	width  int
	height int
}

func stdlibVerdict(data []byte) verdict {
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return verdict{err: err.Error()}
	}
	return verdict{ok: true, width: img.Bounds().Dx(), height: img.Bounds().Dy()}
}

func validateVerdict(data []byte) verdict {
	w, h, err := Validate(bytes.NewReader(data))
	if err != nil {
		return verdict{err: err.Error()}
	}
	return verdict{ok: true, width: w, height: h}
}

func encodeJPEG(t testing.TB, img image.Image, quality int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: quality}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// corpus は標準ライブラリの testdata(プログレッシブ、4:1:0〜4:4:4、CMYK、RGB、グレースケール、
// リスタートマーカー、途中切れなど)と、Go のエンコーダで作った画像。
func corpus(t testing.TB) map[string][]byte {
	t.Helper()
	files, err := filepath.Glob("testdata/*.jpeg")
	if err != nil || len(files) == 0 {
		t.Fatalf("testdata not found: %v", err)
	}
	out := make(map[string][]byte)
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[filepath.Base(f)] = data
	}
	rgba := func(w, h int) image.Image {
		img := image.NewRGBA(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				img.Set(x, y, color.RGBA{uint8(x * 7), uint8(y * 5), uint8(x ^ y), 255})
			}
		}
		return img
	}
	gray := func(w, h int) image.Image {
		img := image.NewGray(image.Rect(0, 0, w, h))
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				img.SetGray(x, y, color.Gray{uint8(x*3 + y)})
			}
		}
		return img
	}
	for _, size := range [][2]int{{1, 1}, {7, 9}, {16, 16}, {33, 17}, {64, 48}, {300, 200}} {
		for _, q := range []int{1, 30, 90, 100} {
			out["enc-rgba-"+itoa(size[0])+"x"+itoa(size[1])+"-q"+itoa(q)] = encodeJPEG(t, rgba(size[0], size[1]), q)
			out["enc-gray-"+itoa(size[0])+"x"+itoa(size[1])+"-q"+itoa(q)] = encodeJPEG(t, gray(size[0], size[1]), q)
		}
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func check(t *testing.T, name string, data []byte) {
	t.Helper()
	want := stdlibVerdict(data)
	got := validateVerdict(data)
	if got != want {
		t.Errorf("%s: Validate=%+v, jpeg.Decode=%+v", name, got, want)
	}
}

func TestValidateMatchesDecode(t *testing.T) {
	cases := 0
	for name, data := range corpus(t) {
		check(t, name, data)
		cases++
	}
	for name, data := range map[string][]byte{
		"empty":       {},
		"soi-only":    {0xff, 0xd8},
		"soi-eoi":     {0xff, 0xd8, 0xff, 0xd9},
		"png-magic":   []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"),
		"garbage":     []byte("not a jpeg at all"),
		"ff-run":      bytes.Repeat([]byte{0xff}, 64),
		"sos-no-sof":  {0xff, 0xd8, 0xff, 0xda, 0x00, 0x08, 0x01, 0x01, 0x00, 0x00, 0x3f, 0x00, 0xff, 0xd9},
		"unknown-mkr": {0xff, 0xd8, 0xff, 0x05, 0x00, 0x02, 0xff, 0xd9},
	} {
		check(t, name, data)
		cases++
	}
	t.Logf("compared %d inputs", cases)
}

// TestValidateTruncations は、すべての画像を1バイトずつ短く切ったものを比べる。
func TestValidateTruncations(t *testing.T) {
	cases := 0
	for name, data := range corpus(t) {
		// 小さい画像は1バイトずつ、大きい画像は13バイトおきに切る(すべて1バイトずつだと約80秒かかる)
		step := 1
		if len(data) > 8000 {
			step = 13
		}
		for n := 0; n < len(data); n += step {
			check(t, name+"[:"+itoa(n)+"]", data[:n])
			cases++
		}
	}
	t.Logf("compared %d truncations", cases)
}

// TestValidateCorruptions は、ランダムなビット反転・バイトの置き換え・挿入・削除をしたものを比べる。
func TestValidateCorruptions(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	cases := 0
	for name, data := range corpus(t) {
		for i := 0; i < 400; i++ {
			mut := append([]byte(nil), data...)
			nOps := 1 + rng.Intn(4)
			for op := 0; op < nOps && len(mut) > 0; op++ {
				pos := rng.Intn(len(mut))
				switch rng.Intn(4) {
				case 0:
					mut[pos] ^= 1 << uint(rng.Intn(8))
				case 1:
					mut[pos] = byte(rng.Intn(256))
				case 2:
					mut = append(mut[:pos], append([]byte{byte(rng.Intn(256))}, mut[pos:]...)...)
				case 3:
					mut = append(mut[:pos], mut[pos+1:]...)
				}
			}
			check(t, name+"#"+itoa(i), mut)
			cases++
		}
	}
	t.Logf("compared %d corruptions", cases)
}

func FuzzValidateMatchesDecode(f *testing.F) {
	for _, data := range corpus(f) {
		f.Add(data)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		// 標準ライブラリが巨大な画像バッファを確保してテストが遅くならないよう、宣言サイズが大きいものは飛ばす
		if cfg, err := jpeg.DecodeConfig(bytes.NewReader(data)); err == nil && cfg.Width*cfg.Height > 1<<22 {
			t.Skip()
		}
		want := stdlibVerdict(data)
		got := validateVerdict(data)
		if got != want {
			t.Errorf("Validate=%+v, jpeg.Decode=%+v", got, want)
		}
	})
}

func BenchmarkValidate(b *testing.B) {
	img := image.NewRGBA(image.Rect(0, 0, 1024, 768))
	for i := range img.Pix {
		img.Pix[i] = byte(i * 31)
	}
	data := encodeJPEG(b, img, 85)
	b.Run("jpeg.Decode", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Validate", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, _, err := Validate(bytes.NewReader(data)); err != nil {
				b.Fatal(err)
			}
		}
	})
}
