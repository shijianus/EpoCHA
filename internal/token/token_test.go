package token

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

var master = bytes.Repeat([]byte{0xa1}, 32)
var prevMaster = bytes.Repeat([]byte{0xb2}, 32)

func newSigner(t *testing.T, previous []byte) *Signer {
	t.Helper()
	s, err := NewSigner(master, previous)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

func chalClaims() Claims {
	return Claims{
		Sid:  7,
		Svid: 3,
		CID:  "cid-abc",
		Exp:  time.Now().Add(3 * time.Minute).Unix(),
		Bits: 18,
		Seed: "aGVsbG8",
	}
}

func TestSignVerifyRoundTrip(t *testing.T) {
	s := newSigner(t, nil)
	now := time.Now()
	tok, err := s.Sign(PurposeChallenge, chalClaims(), now)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if !strings.HasPrefix(tok, prefix+"."+s.CurrentKid()+".") {
		t.Fatalf("线格式不符：%s", tok[:24])
	}
	c, err := s.Verify(PurposeChallenge, tok, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.Sid != 7 || c.CID != "cid-abc" || c.Bits != 18 || c.Seed != "aGVsbG8" || c.Typ != PurposeChallenge {
		t.Fatalf("claims 回读不符：%+v", c)
	}
	if c.Exp <= now.Unix() {
		t.Error("exp 未保留")
	}
}

func TestResponseTokenNeedsRID(t *testing.T) {
	s := newSigner(t, nil)
	now := time.Now()
	if _, err := s.Sign(PurposeResponse, chalClaims(), now); !errors.Is(err, ErrUnusableKey) {
		t.Fatalf("resp 令牌缺 rid 应拒绝，got %v", err)
	}
	c := chalClaims()
	c.RID = "rid-1"
	tok, err := s.Sign(PurposeResponse, c, now)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.Verify(PurposeResponse, tok, now); err != nil || got.RID != "rid-1" {
		t.Fatalf("resp 验证失败：%v %+v", err, got)
	}
}

func TestPurposeConfusionRejected(t *testing.T) {
	s := newSigner(t, nil)
	now := time.Now()
	chal, err := s.Sign(PurposeChallenge, chalClaims(), now)
	if err != nil {
		t.Fatal(err)
	}
	c := chalClaims()
	c.RID = "rid-1"
	resp, err := s.Sign(PurposeResponse, c, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(PurposeResponse, chal, now); !errors.Is(err, ErrMalformed) {
		t.Errorf("chal 当 resp 用应拒绝，got %v", err)
	}
	if _, err := s.Verify(PurposeChallenge, resp, now); !errors.Is(err, ErrMalformed) {
		t.Errorf("resp 当 chal 用应拒绝，got %v", err)
	}
}

// 签名覆盖全部 payload 字节：任何一位翻转都必须验签失败。
func TestEveryPayloadBitFlipRejected(t *testing.T) {
	s := newSigner(t, nil)
	now := time.Now()
	tok, err := s.Sign(PurposeChallenge, chalClaims(), now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(tok, ".", 4)
	payload := parts[2]
	tag := parts[3]
	flipped := 0
	for i := 0; i < len(payload); i++ {
		for _, alt := range []byte{'A', 'B', 'Z', '0', '-', '_'} {
			if payload[i] == alt {
				continue
			}
			mut := parts[0] + "." + parts[1] + "." + payload[:i] + string(alt) + payload[i+1:] + "." + tag
			if _, err := s.Verify(PurposeChallenge, mut, now); !errors.Is(err, ErrMalformed) {
				t.Fatalf("payload 第 %d 位改为 %q 竟通过验证：%v", i, alt, err)
			}
			flipped++
		}
	}
	if flipped < len(payload) {
		t.Fatalf("变异样本过少：%d", flipped)
	}
	// tag 同样逐位变异
	for i := 0; i < len(tag); i++ {
		for _, alt := range []byte{'A', 'q', '9'} {
			if tag[i] == alt {
				continue
			}
			mut := parts[0] + "." + parts[1] + "." + payload + "." + tag[:i] + string(alt) + tag[i+1:]
			if _, err := s.Verify(PurposeChallenge, mut, now); !errors.Is(err, ErrMalformed) {
				t.Fatalf("tag 第 %d 位变异竟通过：%v", i, err)
			}
		}
	}
}

func TestExpiredAndFutureTokens(t *testing.T) {
	s := newSigner(t, nil)
	now := time.Now()
	c := chalClaims()
	c.Exp = now.Add(60 * time.Second).Unix()
	tok, err := s.Sign(PurposeChallenge, c, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(PurposeChallenge, tok, now.Add(61*time.Second)); !errors.Is(err, ErrExpired) {
		t.Errorf("过期应报 ErrExpired，got %v", err)
	}
	if _, err := s.Verify(PurposeChallenge, tok, now); err != nil {
		t.Errorf("窗口内应通过，got %v", err)
	}
	future, err := s.Sign(PurposeChallenge, chalClaims(), now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(PurposeChallenge, future, now); !errors.Is(err, ErrMalformed) {
		t.Errorf("iat 严重超前应拒绝，got %v", err)
	}
}

func TestUnknownKidAndRotation(t *testing.T) {
	s := newSigner(t, prevMaster)
	now := time.Now()
	tok, err := s.Sign(PurposeChallenge, chalClaims(), now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(tok, ".", 4)
	other := NewSignerOnlyOnPrevious(t)
	oldTok, err := other.Sign(PurposeChallenge, chalClaims(), now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Verify(PurposeChallenge, oldTok, now); err != nil {
		t.Errorf("previous kid 应在轮换窗口内可用：%v", err)
	}
	fresh, err := NewSigner(prevMaster, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.Verify(PurposeChallenge, tok, now); !errors.Is(err, ErrUnknownKid) {
		t.Errorf("未登记的 kid 应报 ErrUnknownKid，got %v", err)
	}
	bad := prefix + "." + strings.Repeat("0", len(parts[1])) + "." + parts[2] + "." + parts[3]
	if _, err := s.Verify(PurposeChallenge, bad, now); !errors.Is(err, ErrUnknownKid) {
		t.Errorf("伪造 kid 应报 ErrUnknownKid，got %v", err)
	}
}

func NewSignerOnlyOnPrevious(t *testing.T) *Signer {
	t.Helper()
	// 模拟"旧主密钥仍是 current"的上一版进程
	s, err := NewSigner(prevMaster, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestMalformedInputs(t *testing.T) {
	s := newSigner(t, nil)
	now := time.Now()
	valid, err := s.Sign(PurposeChallenge, chalClaims(), now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(valid, ".", 4)

	cases := map[string]string{
		"空":                "",
		"段数不足":             prefix + "." + parts[1] + "." + parts[2],
		"版本错误":             "v2." + parts[1] + "." + parts[2] + "." + parts[3],
		"payload 空":        prefix + "." + parts[1] + ".." + parts[3],
		"tag 空":            prefix + "." + parts[1] + "." + parts[2] + ".",
		"kid 空":            prefix + ".." + parts[2] + "." + parts[3],
		"payload 非 base64": prefix + "." + parts[1] + ".@@@@" + "." + parts[3],
		"tag 非 base64":     prefix + "." + parts[1] + "." + parts[2] + ".!!!!",
		"尾部多余段":            valid + ".extra",
		"超长":               prefix + "." + parts[1] + "." + strings.Repeat("A", maxTokenBytes) + "." + parts[3],
	}
	for name, tok := range cases {
		if _, err := s.Verify(PurposeChallenge, tok, now); err == nil {
			t.Errorf("%s：应被拒绝", name)
		}
	}
}

// 未知字段必须拒绝：防止同一密钥下被塞入额外语义。
func TestUnknownClaimFieldRejected(t *testing.T) {
	s := newSigner(t, nil)
	now := time.Now()
	body, err := json.Marshal(map[string]any{
		"typ": PurposeChallenge, "kid": s.CurrentKid(), "sid": 7, "cid": "cid-abc",
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "bits": 18,
		"seed": "aGVsbG8", "scope": "admin",
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	tag := computeTag(master, PurposeChallenge, payload)
	tok := prefix + "." + s.CurrentKid() + "." + payload + "." + tag
	if _, err := s.Verify(PurposeChallenge, tok, now); !errors.Is(err, ErrMalformed) {
		t.Fatalf("未知字段应拒绝，got %v", err)
	}
	// 反向确认：同一段 payload 去掉未知字段后能通过，说明失败原因确实是字段校验
	good, err := json.Marshal(Claims{Typ: PurposeChallenge, Kid: s.CurrentKid(), Sid: 7, Svid: 3, CID: "cid-abc",
		Iat: now.Unix(), Exp: now.Add(time.Minute).Unix(), Bits: 18, Seed: "aGVsbG8"})
	if err != nil {
		t.Fatal(err)
	}
	p2 := base64.RawURLEncoding.EncodeToString(good)
	if _, err := s.Verify(PurposeChallenge, prefix+"."+s.CurrentKid()+"."+p2+"."+computeTag(master, PurposeChallenge, p2), now); err != nil {
		t.Fatalf("合法 token 竟失败：%v", err)
	}
}

func TestSignerRejectsWeakOrDuplicateKeys(t *testing.T) {
	if _, err := NewSigner(bytes.Repeat([]byte{1}, 16), nil); !errors.Is(err, ErrUnusableKey) {
		t.Errorf("主密钥过短应拒绝，got %v", err)
	}
	if _, err := NewSigner(master, master); !errors.Is(err, ErrUnusableKey) {
		t.Errorf("新旧密钥相同应拒绝，got %v", err)
	}
	s := newSigner(t, nil)
	if _, err := s.Sign("ops", chalClaims(), time.Now()); !errors.Is(err, ErrUnusableKey) {
		t.Errorf("未知用途应拒绝，got %v", err)
	}
	c := chalClaims()
	c.Sid = 0
	if _, err := s.Sign(PurposeChallenge, c, time.Now()); !errors.Is(err, ErrUnusableKey) {
		t.Error("缺站点绑定应拒绝")
	}
	c = chalClaims()
	c.Bits = 0
	if _, err := s.Sign(PurposeChallenge, c, time.Now()); !errors.Is(err, ErrUnusableKey) {
		t.Error("缺难度应拒绝")
	}
}

// payload 超过上限时不得签发（避免构造超大 token）。
func TestSignRejectsOversizePayload(t *testing.T) {
	s := newSigner(t, nil)
	c := chalClaims()
	c.Seed = strings.Repeat("A", maxPayloadBytes)
	if _, err := s.Sign(PurposeChallenge, c, time.Now()); !errors.Is(err, ErrUnusableKey) {
		t.Fatalf("超长 payload 应拒绝，got %v", err)
	}
}

func TestVerifyHasBoundedRead(t *testing.T) {
	s := newSigner(t, nil)
	now := time.Now()
	// 巨大但签名正确的 payload 也必须被 token 总长度上限挡住
	big, err := json.Marshal(Claims{Typ: PurposeChallenge, Kid: s.CurrentKid(), Sid: 7, Svid: 3, CID: strings.Repeat("c", 1),
		Iat: now.Unix(), Exp: now.Add(time.Minute).Unix(), Bits: 18, Seed: strings.Repeat("A", maxPayloadBytes)})
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(big)) > maxPayloadBytes {
		payload := base64.RawURLEncoding.EncodeToString(big)
		tok := prefix + "." + s.CurrentKid() + "." + payload + "." + computeTag(master, PurposeChallenge, payload)
		if _, err := s.Verify(PurposeChallenge, tok, now); err == nil {
			t.Fatal("超长 token 必须拒绝")
		}
	}
	// io.LimitReader 生效的等价断言：解码器不会读超过上限
	if _, err := io.ReadAll(io.LimitReader(bytes.NewReader(big), maxPayloadBytes)); err != nil {
		t.Fatal(err)
	}
}

func TestSplitTokenRejectsEmptySegments(t *testing.T) {
	for _, tok := range []string{"v1...x", ".a.b.c", "v1.a.b.", "v1.a.b", "onlyone"} {
		if got := splitToken(tok); got != nil && len(got) == 4 {
			for _, p := range got {
				if p == "" {
					t.Fatalf("%q 分段含空串", tok)
				}
			}
		}
	}
}

// 站点版本是签名覆盖的一部分：停用再启用站点后，旧 token 的 svid 对不上当前版本即被拒。
// 这里证明"签名层面就已拒绝"，无需依赖库里还留着那条状态。
func TestSiteVersionBinding(t *testing.T) {
	s := newSigner(t, nil)
	now := time.Now()
	tok, err := s.Sign(PurposeChallenge, chalClaims(), now)
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Verify(PurposeChallenge, tok, now)
	if err != nil {
		t.Fatal(err)
	}
	if c.Svid != 3 {
		t.Fatalf("svid 未签名往返：%d", c.Svid)
	}
	// 缺少 svid（阶段 1 线格式）必须在验签后被拒，而不是当成 0 放行
	master := bytes.Repeat([]byte{0x5a}, 32)
	legacy, err := json.Marshal(map[string]any{
		"typ": PurposeChallenge, "kid": s.CurrentKid(), "sid": 7, "cid": "cid-abc",
		"iat": now.Unix(), "exp": now.Add(time.Minute).Unix(), "bits": 18, "seed": "aGVsbG8",
	})
	if err != nil {
		t.Fatal(err)
	}
	p := base64.RawURLEncoding.EncodeToString(legacy)
	if _, err := s.Verify(PurposeChallenge, prefix+"."+s.CurrentKid()+"."+p+"."+computeTag(master, PurposeChallenge, p), now); !errors.Is(err, ErrMalformed) {
		t.Fatalf("无 svid 的旧线格式应拒绝，got %v", err)
	}
	// svid<=0 也不得签发
	c2 := chalClaims()
	c2.Svid = 0
	if _, err := s.Sign(PurposeChallenge, c2, now); !errors.Is(err, ErrUnusableKey) {
		t.Fatalf("缺站点版本不应签发，got %v", err)
	}
}
