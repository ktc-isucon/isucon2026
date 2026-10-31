package isureal

import (
	"bytes"
	"crypto/sha256"
	"image/jpeg"
	"sync"
)

// jpegCheck はアップロードされた JPEG の検証結果。
// decoded が false のときは、上限超えのため全体のデコードを省略したことを表す(このとき ok は未確定)。
type jpegCheck struct {
	ok      bool // jpeg.Decode が成功するか
	decoded bool
	width   int
	height  int
}

const jpegCheckCacheLimit = 4096

var jpegChecks = struct {
	sync.Mutex
	m map[[sha256.Size]byte]jpegCheck
}{m: make(map[[sha256.Size]byte]jpegCheck)}

// checkJPEG は jpeg.Decode と同じ受理判定と縦横の大きさを返す。
//   - jpeg.DecodeConfig が失敗する入力は jpeg.Decode も必ず失敗する(同じパーサが SOF まで同じ処理をする)ので、
//     全体をデコードせずに失敗とする。
//   - skipOver > 0 のとき、宣言された縦横のどちらかが skipOver を超えるなら全体のデコードを省略する
//     (呼び出し側がデコードの成否に関わらず同じ応答を返す場合だけ使う)。
//   - 同じバイト列の結果は決定的なので、SHA-256 をキーにメモする。
func checkJPEG(data []byte, skipOver int) jpegCheck {
	key := sha256.Sum256(data)
	jpegChecks.Lock()
	cached, hit := jpegChecks.m[key]
	jpegChecks.Unlock()
	if hit && (cached.decoded || skipOver > 0) {
		return cached
	}

	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		result := jpegCheck{ok: false, decoded: true}
		storeJPEGCheck(key, result)
		return result
	}
	if skipOver > 0 && (cfg.Width > skipOver || cfg.Height > skipOver) {
		return jpegCheck{decoded: false, width: cfg.Width, height: cfg.Height}
	}

	img, err := jpeg.Decode(bytes.NewReader(data))
	result := jpegCheck{ok: err == nil, decoded: true}
	if err == nil {
		result.width, result.height = img.Bounds().Dx(), img.Bounds().Dy()
	}
	storeJPEGCheck(key, result)
	return result
}

func storeJPEGCheck(key [sha256.Size]byte, result jpegCheck) {
	jpegChecks.Lock()
	defer jpegChecks.Unlock()
	if len(jpegChecks.m) >= jpegCheckCacheLimit {
		jpegChecks.m = make(map[[sha256.Size]byte]jpegCheck)
	}
	jpegChecks.m[key] = result
}
