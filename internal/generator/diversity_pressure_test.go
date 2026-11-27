package generator

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image/png"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/shijianus/EpoCHA/internal/eval"
)

func pressureConstructor[T Generator](f func([]byte) (T, error)) func([]byte) (Generator, error) {
	return func(k []byte) (Generator, error) { return f(k) }
}

// Fingerprints cover the actual public DTO and asset, never the private verifier.
// Shared image metadata is counted separately; it is not a duplicate picture.
func TestGeneratorDiversityPressure(t *testing.T) {
	factories := []func([]byte) (Generator, error){
		pressureConstructor(NewArithmeticBaseline), pressureConstructor(NewTextImage),
		pressureConstructor(NewTextImageV2), pressureConstructor(NewTextImageV3),
		pressureConstructor(NewShapeGrid), pressureConstructor(NewShapeCount),
		pressureConstructor(NewParallax), pressureConstructor(NewBankShot),
		pressureConstructor(NewAccessibleChoice), pressureConstructor(NewEnergyWindow),
		pressureConstructor(NewPipeConnect), pressureConstructor(NewIntersectionPoint),
		pressureConstructor(NewTileRotation), pressureConstructor(NewSymbolPath),
		pressureConstructor(NewCrowdFind), pressureConstructor(NewVisualRank),
		pressureConstructor(NewProgressiveTarget), pressureConstructor(NewShellTrack),
		pressureConstructor(NewSilhouetteAssembly),
	}
	if len(factories) != 19 {
		t.Fatal("pressure constructor coverage changed")
	}
	global := make(map[[32]byte]string)
	var globalMu sync.Mutex
	ids := make(map[string]bool)
	for _, factory := range factories {
		g, err := factory(testMacKey())
		if err != nil {
			t.Fatal(err)
		}
		if ids[g.ID()] {
			t.Fatalf("duplicate constructor %s", g.ID())
		}
		ids[g.ID()] = true
		t.Run(g.ID(), func(t *testing.T) {
			// Each constructor has independent state. Honor go test's bounded
			// -parallel setting so race instrumentation can use the available CPUs.
			t.Parallel()
			seen := make(map[[32]byte]bool)
			metadata := make(map[[32]byte]bool)
			var difficulties [5]int
			var duplicates, cross, metaDuplicates, deterministic, replays, random int
			var maxPayload, maxAsset, maxVerifier int
			generate := func(d int, entropy io.Reader) (Question, []byte, [32]byte, [32]byte) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				q, err := g.Generate(ctx, Params{Difficulty: d}, entropy)
				if err != nil {
					t.Fatalf("d%d Generate: %v", d, err)
				}
				if ctx.Err() != nil {
					t.Fatalf("d%d generation deadline: %v", d, ctx.Err())
				}
				raw, err := json.Marshal(q.Payload)
				if err != nil {
					t.Fatal(err)
				}
				if q.Payload == nil || len(raw) <= 2 || bytes.Equal(raw, []byte("null")) || len(q.Verifier) == 0 {
					t.Fatal("empty payload/verifier")
				}
				if len(raw) > 8192 || len(q.Asset) > 65536 || len(q.Verifier) > 4096 {
					t.Fatalf("byte limit: payload=%d asset=%d verifier=%d", len(raw), len(q.Asset), len(q.Verifier))
				}
				public, err := g.SanitizePublic(raw)
				if err != nil || len(public) == 0 {
					t.Fatalf("SanitizePublic: %v", err)
				}
				publicJSON, err := json.Marshal(public)
				if err != nil {
					t.Fatal(err)
				}
				if len(publicJSON) > 8192 {
					t.Fatalf("public DTO exceeds limit: %d", len(publicJSON))
				}
				maxPayload = max(maxPayload, len(raw), len(publicJSON))
				maxAsset = max(maxAsset, len(q.Asset))
				maxVerifier = max(maxVerifier, len(q.Verifier))
				if (len(q.Asset) > 0) != (q.MimeType != "") {
					t.Fatal("asset/MIME pair mismatch")
				}
				switch g.Media() {
				case MediaText:
					if len(q.Asset) != 0 || q.MimeType != "" {
						t.Fatal("text carries asset")
					}
				case MediaImage:
					if len(q.Asset) == 0 || q.MimeType != "image/png" {
						t.Fatal("missing PNG asset/MIME")
					}
					var dims struct {
						W     int    `json:"image_width"`
						H     int    `json:"image_height"`
						Mode  string `json:"render_mode"`
						Cols  int    `json:"frame_cols"`
						Rows  int    `json:"frame_rows"`
						Count int    `json:"frame_count"`
					}
					if err := json.Unmarshal(publicJSON, &dims); err != nil {
						t.Fatal(err)
					}
					w, h := dims.W, dims.H
					if dims.Mode == "sprite_sheet" {
						if dims.Cols < 1 || dims.Cols > 8 || dims.Rows < 1 || dims.Rows > 8 || dims.Count < 2 || dims.Count > 64 || dims.Cols*dims.Rows < dims.Count {
							t.Fatal("invalid sprite geometry")
						}
						w *= dims.Cols
						h *= dims.Rows
					}
					img, err := png.Decode(bytes.NewReader(q.Asset))
					if err != nil {
						t.Fatalf("full PNG decode: %v", err)
					}
					if w <= 0 || h <= 0 || img.Bounds().Dx() != w || img.Bounds().Dy() != h {
						t.Fatalf("PNG=%v public=%dx%d", img.Bounds(), w, h)
					}
				default:
					t.Fatalf("unknown media %q", g.Media())
				}
				hash := sha256.New()
				for _, b := range [][]byte{publicJSON, []byte(q.MimeType), q.Asset} {
					_ = binary.Write(hash, binary.BigEndian, uint64(len(b)))
					_, _ = hash.Write(b)
				}
				var fp [32]byte
				copy(fp[:], hash.Sum(nil))
				return q, raw, fp, sha256.Sum256(publicJSON)
			}
			record := func(fp, meta [32]byte) {
				if seen[fp] {
					duplicates++
				}
				seen[fp] = true
				globalMu.Lock()
				if owner, ok := global[fp]; ok && owner != g.ID() {
					cross++
				}
				global[fp] = g.ID()
				globalMu.Unlock()
				if g.Media() == MediaImage {
					if metadata[meta] {
						metaDuplicates++
					}
					metadata[meta] = true
				}
			}
			for i := 0; i < 64; i++ {
				d := i%5 + 1
				seed := []byte(fmt.Sprintf("diversity-pressure/v1/%s/%d", g.ID(), i))
				e, err := eval.NewDeterministic(seed)
				if err != nil {
					t.Fatal(err)
				}
				q, raw, fp, meta := generate(d, e)
				deterministic++
				record(fp, meta)
				e, err = eval.NewDeterministic(seed)
				if err != nil {
					t.Fatal(err)
				}
				r, rr, rfp, _ := generate(d, e)
				replays++
				if !bytes.Equal(raw, rr) || !bytes.Equal(q.Asset, r.Asset) || !bytes.Equal(q.Verifier, r.Verifier) || q.MimeType != r.MimeType || fp != rfp {
					t.Fatalf("byte replay differs d%d sample%d", d, i)
				}
			}
			for i := 0; i < 1000; i++ {
				d := i%5 + 1
				_, _, fp, meta := generate(d, rand.Reader)
				random++
				difficulties[d-1]++
				record(fp, meta)
			}
			t.Logf("PRESSURE id=%s deterministic=%d replay=%d random=%d difficulties=%v unique=%d full_duplicates=%d cross_duplicates=%d metadata_duplicates=%d max_bytes=%d/%d/%d", g.ID(), deterministic, replays, random, difficulties, len(seen), duplicates, cross, metaDuplicates, maxPayload, maxAsset, maxVerifier)
			if deterministic != 64 || replays != 64 || random != 1000 || difficulties != [5]int{200, 200, 200, 200, 200} {
				t.Fatal("incomplete pressure coverage")
			}
			if duplicates != 0 || cross != 0 {
				t.Errorf("public surface collision: within=%d cross=%d", duplicates, cross)
			}
		})
	}
}
