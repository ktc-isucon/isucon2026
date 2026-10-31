package isureal

import (
	"bytes"
	"image/jpeg"

	"example.com/isureal-company/isureal/internal/jpegcheck"
)

// jpegCheck はアップロードされた JPEG の検証結果。
// decoded が false のときは、上限超えのため全体の検証を省略したことを表す(このとき ok は未確定)。
type jpegCheck struct {
	ok      bool // jpeg.Decode が成功するか
	decoded bool
	width   int
	height  int
}

// checkJPEG は jpeg.Decode と同じ受理判定と縦横の大きさを返す。
//   - jpeg.DecodeConfig が失敗する入力は jpeg.Decode も必ず失敗する(同じパーサが SOF まで同じ処理をする)ので、
//     全体を検証せずに失敗とする。
//   - skipOver > 0 のとき、宣言された縦横のどちらかが skipOver を超えるなら全体の検証を省略する
//     (呼び出し側がデコードの成否に関わらず同じ応答を返す場合だけ使う)。
//   - 全体の検証は、画素を作らない jpegcheck.Validate で行う(jpeg.Decode と同じ条件でエラーを返す)。
//
// 以前は SHA-256 をキーに判定をメモしていたが、同じ画像の再送がほぼなく当たらない一方で、
// キーの計算が isu0 の CPU の約19%を使っていたのでやめた。
func checkJPEG(data []byte, skipOver int) jpegCheck {
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return jpegCheck{ok: false, decoded: true}
	}
	if skipOver > 0 && (cfg.Width > skipOver || cfg.Height > skipOver) {
		return jpegCheck{decoded: false, width: cfg.Width, height: cfg.Height}
	}

	width, height, err := jpegcheck.Validate(bytes.NewReader(data))
	result := jpegCheck{ok: err == nil, decoded: true}
	if err == nil {
		result.width, result.height = width, height
	}
	return result
}
