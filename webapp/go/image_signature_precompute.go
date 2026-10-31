package isureal

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 画像URLの exp は60秒の枠ごとに変わるので、枠が切り替わった瞬間にメモが一斉に外れ、
// 応答の組み立てで HMAC 1000回の計算が集中する。
// そこで、今の枠でURLを作った (鍵, 画像ID) を覚えておき、枠が切り替わる少し前から、
// CPU が空いているときだけ次の枠の exp の署名を先に計算してメモに入れる。
// 署名の値は (鍵, 画像ID, exp) だけで決まり、形式も exp の決め方も変えないので、応答は変わらない。
// CPU の総量は減らないが、空いている時間に計算を前倒しして、切り替わり直後の負荷の山を平らにする。

const (
	// 枠が切り替わるこの時間前から、次の枠の署名を先に計算する
	imageSignaturePrecomputeLead = 20 * time.Second
	// 1つの枠で覚えておく (鍵, 画像ID) の上限(1件あたり100バイト前後)
	imageSignaturePrecomputeLimit = 100000
	// 一度に計算する件数(1件約0.1ms)。この件数ごとに CPU の空きを確かめ直す
	imageSignaturePrecomputeBatch = 16
	// システム全体の CPU の空きがこの割合以上のときだけ計算する(2コアなら約0.7コア分の空き)
	imageSignaturePrecomputeMinIdle = 0.35
)

type imageSignatureTargetKey struct {
	key     string
	imageID string
}

var imageSignaturePrecompute = struct {
	sync.Mutex
	exp     int64                                // targets を記録している枠の exp
	targets map[imageSignatureTargetKey]struct{} // 今の枠でURLを作った (鍵, 画像ID)
	done    map[imageSignatureTargetKey]struct{} // 次の枠の署名を計算し終えたもの
	once    sync.Once
}{}

// imageSignatureForURL は署名URLを作るときの署名を返し、次の枠の先回り計算の対象として覚える。
// 検証(getImageHandler)では使わない(クライアントが送ってきた exp の分は覚えない)。
func imageSignatureForURL(key []byte, imageID string, exp int64) []byte {
	recordImageSignatureTarget(key, imageID, exp)
	return imageSignature(key, imageID, exp)
}

func recordImageSignatureTarget(key []byte, imageID string, exp int64) {
	p := &imageSignaturePrecompute
	p.once.Do(func() { go runImageSignaturePrecompute() })
	p.Lock()
	defer p.Unlock()
	if exp > p.exp {
		// 新しい枠になった。前の枠の記録は、その枠の切り替わりまでに使い終わっている
		p.exp = exp
		p.targets = make(map[imageSignatureTargetKey]struct{})
		p.done = make(map[imageSignatureTargetKey]struct{})
	} else if exp < p.exp {
		return
	}
	if len(p.targets) >= imageSignaturePrecomputeLimit {
		return
	}
	p.targets[imageSignatureTargetKey{key: string(key), imageID: imageID}] = struct{}{}
}

// nextImageURLExpSwitch は、今の枠の exp(cur) が次の枠の exp(cur+粒度) に切り替わる時刻を返す。
// imageURLExp は now+24h を粒度で切り上げるので、now+24h が cur を超えた時点で切り替わる。
func nextImageURLExpSwitch(cur int64) time.Time {
	return time.Unix(cur, 0).Add(-imageURLLifetime)
}

func runImageSignaturePrecompute() {
	idle := newCPUIdleSampler()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		precomputeNextImageSignatures(time.Now, idle.enoughIdle)
	}
}

// precomputeNextImageSignatures は、枠の切り替わりが近ければ、次の枠の署名を計算する。計算した件数を返す。
// enoughIdle が false を返したら、その時点でやめる(続きは次の呼び出しで行う)。clock は今の時刻を返す。
func precomputeNextImageSignatures(clock func() time.Time, enoughIdle func() bool) int {
	p := &imageSignaturePrecompute
	now := clock()
	cur := imageURLExp(now)
	remaining := nextImageURLExpSwitch(cur).Sub(now)
	if remaining > imageSignaturePrecomputeLead || remaining <= 0 {
		return 0
	}
	next := cur + imageURLExpGranularity

	p.Lock()
	if p.exp != cur {
		p.Unlock()
		return 0
	}
	pending := make([]imageSignatureTargetKey, 0, len(p.targets)-len(p.done))
	for t := range p.targets {
		if _, ok := p.done[t]; !ok {
			pending = append(pending, t)
		}
	}
	p.Unlock()

	computed := 0
	for start := 0; start < len(pending); start += imageSignaturePrecomputeBatch {
		if !enoughIdle() {
			break
		}
		// 計算の途中で枠が切り替わったら、残りは応答の組み立てで求める分と同じなので、そちらに任せる
		if imageURLExp(clock()) != cur {
			break
		}
		end := min(start+imageSignaturePrecomputeBatch, len(pending))
		for _, t := range pending[start:end] {
			imageSignature([]byte(t.key), t.imageID, next)
		}
		computed += end - start
		p.Lock()
		if p.exp == cur {
			for _, t := range pending[start:end] {
				p.done[t] = struct{}{}
			}
		}
		p.Unlock()
	}
	return computed
}

// cpuIdleSampler は /proc/stat から、前回からのシステム全体の CPU の空きの割合を求める。
// アプリと同じサーバーでレプリカの PostgreSQL も動くので、プロセス単位ではなくシステム全体で見る。
type cpuIdleSampler struct {
	lastIdle, lastTotal uint64
	ok                  bool
}

func newCPUIdleSampler() *cpuIdleSampler {
	s := &cpuIdleSampler{}
	s.lastIdle, s.lastTotal, s.ok = readCPUStat()
	return s
}

// enoughIdle は、前回の呼び出しからの CPU の空きが十分なら true を返す。
// /proc/stat が読めない環境(Linux 以外)では、先回り計算をしない。
func (s *cpuIdleSampler) enoughIdle() bool {
	time.Sleep(20 * time.Millisecond) // 測る区間をつくり、計算の合間に CPU を譲る
	idle, total, ok := readCPUStat()
	if !ok || !s.ok {
		s.ok = ok
		return false
	}
	dIdle, dTotal := idle-s.lastIdle, total-s.lastTotal
	s.lastIdle, s.lastTotal = idle, total
	if dTotal == 0 {
		return false
	}
	return float64(dIdle)/float64(dTotal) >= imageSignaturePrecomputeMinIdle
}

func readCPUStat() (idle, total uint64, ok bool) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	if !sc.Scan() {
		return 0, 0, false
	}
	fields := strings.Fields(sc.Text())
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	for i, field := range fields[1:] {
		v, err := strconv.ParseUint(field, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		total += v
		if i == 3 || i == 4 { // idle と iowait
			idle += v
		}
	}
	return idle, total, true
}
