package isureal

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"testing"
)

// 旧実装の判定: jpeg.Decode の成否と、成功時の縦横
func legacyJPEGVerdict(data []byte, limit int) (decodeOK, withinLimit bool) {
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		return false, false
	}
	return true, img.Bounds().Dx() <= limit && img.Bounds().Dy() <= limit
}

func encodeTestJPEG(t testing.TB, w, h int, progressiveLike bool) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), uint8(x + y), 255})
		}
	}
	var buf bytes.Buffer
	q := 90
	if progressiveLike {
		q = 30
	}
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: q}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// SOF0 の縦横を書き換えて「巨大サイズ宣言」を作る
func withDeclaredSize(t testing.TB, data []byte, w, h int) []byte {
	t.Helper()
	out := append([]byte(nil), data...)
	for i := 2; i+9 < len(out); i++ {
		if out[i] == 0xFF && (out[i+1] == 0xC0 || out[i+1] == 0xC2) {
			binary.BigEndian.PutUint16(out[i+5:], uint16(h))
			binary.BigEndian.PutUint16(out[i+7:], uint16(w))
			return out
		}
	}
	t.Fatal("SOF not found")
	return nil
}

func TestCheckJPEGMatchesDecode(t *testing.T) {
	small := encodeTestJPEG(t, 64, 48, false)
	big := encodeTestJPEG(t, 600, 300, false)
	var pngBuf bytes.Buffer
	_ = png.Encode(&pngBuf, image.NewRGBA(image.Rect(0, 0, 8, 8)))

	cases := map[string][]byte{
		"valid-small":         small,
		"valid-low-quality":   encodeTestJPEG(t, 100, 100, true),
		"valid-over-512":      big,
		"truncated-half":      small[:len(small)/2],
		"truncated-tail":      small[:len(small)-2],
		"header-only":         small[:20],
		"png":                 pngBuf.Bytes(),
		"empty":               {},
		"declared-huge":       withDeclaredSize(t, small, 6000, 6000),
		"declared-5000":       withDeclaredSize(t, small, 5000, 10),
		"declared-huge-trunc": withDeclaredSize(t, small, 6000, 6000)[:len(small)/2],
		"garbage":             []byte("not a jpeg at all"),
	}
	for _, limit := range []int{512, 4096} {
		for name, data := range cases {
			wantOK, wantWithin := legacyJPEGVerdict(data, limit)
			for _, skip := range []int{0, limit} {
				// メモの有無で結果が変わらないことも見るため2回呼ぶ
				for round := 0; round < 2; round++ {
					got := checkJPEG(data, skip)
					// post/realmoji の判定(skip=limit): 400 になるか
					reject := !got.decoded || !got.ok || got.width > limit || got.height > limit
					if reject != !(wantOK && wantWithin) {
						t.Errorf("%s limit=%d skip=%d round=%d: reject=%v, legacy accept=%v", name, limit, skip, round, reject, wantOK && wantWithin)
					}
					// profile の判定(skip=0): abuse を付ける「デコード失敗」が一致するか
					if skip == 0 && got.ok != wantOK {
						t.Errorf("%s limit=%d: ok=%v, legacy=%v", name, limit, got.ok, wantOK)
					}
				}
			}
		}
	}
}

func BenchmarkCheckJPEGCached(b *testing.B) {
	data := encodeTestJPEG(b, 1024, 768, false)
	checkJPEG(data, 4096)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		checkJPEG(data, 4096)
	}
}

func BenchmarkLegacyJPEGDecode(b *testing.B) {
	data := encodeTestJPEG(b, 1024, 768, false)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		legacyJPEGVerdict(data, 4096)
	}
}
