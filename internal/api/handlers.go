package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/shijianus/EpoCHA/internal/dispatch"
	"github.com/shijianus/EpoCHA/internal/generator"
	"github.com/shijianus/EpoCHA/internal/pow"
	"github.com/shijianus/EpoCHA/internal/risk"
	"github.com/shijianus/EpoCHA/internal/store"
	"github.com/shijianus/EpoCHA/internal/token"
)

type challengeRequest struct {
	SiteKey       string      `json:"site_key"`
	Lang          string      `json:"lang"`
	Accessibility bool        `json:"accessibility,omitempty"`
	Device        *deviceHint `json:"device,omitempty"`
	Session       string      `json:"session,omitempty"`
}

type deviceHint struct {
	Pointer       string `json:"pointer,omitempty"` // fine | coarse
	ReducedMotion bool   `json:"reduced_motion,omitempty"`
}

type powPayload struct {
	Algo string `json:"algo"`
	Seed string `json:"seed"`
	Bits uint8  `json:"bits"`
}

type challengeResponse struct {
	Token     string     `json:"token"`
	ExpiresIn int64      `json:"expires_in"`
	Pow       powPayload `json:"pow"`
	// Mode 让业务方知道自己走的是哪条路径；pow_only 站点省略 question 字段。
	Mode     string         `json:"mode"`
	Question map[string]any `json:"question,omitempty"`
}

type redeemRequest struct {
	Token    string `json:"token"`
	PowNonce string `json:"pow_nonce"`
	// Answer 只在 pow_question 下必填。它**不是**安全边界：真正的判定在服务端库里。
	Answer string `json:"answer"`
	// Telemetry 是量化摘要（ADR-027），只做弱证据；不参与显式判定。
	Telemetry *risk.Telemetry `json:"telemetry,omitempty"`
}

type redeemResponse struct {
	Response  string `json:"response"`
	ExpiresIn int64  `json:"expires_in"`
}

// --- 来源与 CORS ---

// clientSiteKey 取"客户端在 URL 上（或预检用的头里）声明的站点标识"。
//
// 为什么需要它：JSON 请求会触发 CORS 预检，而**预检看不见请求体**，只能从查询参数或
// `X-Epocha-Site-Key` 认站点（见 preflightOrigin）。如果实际操作时改读请求体里的 site_key，
// 就出现"预检按 A 站授权、实际落到 B 站"的歧义。规则：**两处都给出时必须一致**，
// 不一致一律拒绝；不实现成"以某一处为准"，因为选哪一处都会留下一条可被利用的歧义。
//
// 同一个参数出现多个不同值也算冲突：取哪一个取决于解析顺序，那不是语义。
func clientSiteKey(r *http.Request) (string, bool) {
	vals, ok := r.URL.Query()["site_key"]
	if !ok || len(vals) == 0 {
		if h := r.Header.Get("X-Epocha-Site-Key"); h != "" {
			return h, false
		}
		return "", false
	}
	first := vals[0]
	for _, v := range vals[1:] {
		if v != first {
			return "", true
		}
	}
	return first, false
}

// siteKeyConsistent 判断 URL 声明与权威站点标识（体里的 / token 里的）是否可接受。
// 未声明（空）时放行：非浏览器客户端本来就没有预检，这条规则不该拦住它们。
func siteKeyConsistent(declared string, conflict bool, authoritative string) bool {
	if conflict {
		return false
	}
	return declared == "" || declared == authoritative
}

// originCheck 是浏览器来源裁决的结果。
type originCheck struct {
	// Canonical 是规范化后的 Origin（无 Origin 头时为 ""），会写进 challenge 行并被签名绑定。
	Canonical string
	// Allowed 表示该来源可以通过站点策略。
	Allowed bool
	// CORS 为 true 时才对响应挂 Access-Control-* 头。
	CORS bool
	// Reason 只用于日志，绝不进响应（红线 8）。
	Reason string
}

// checkOrigin 实现 DESIGN §10.3 的来源策略：
//   - 有 Origin：必须是合法规范形式且在站点允许列表里，否则拒绝（不做后缀匹配、不自动放行子域）。
//   - 无 Origin：只有站点显式打开 allow_missing_origin 才放行（演示客户端路径），不对所有站点静默放开。
//
// 必须记住的边界：Origin 由浏览器强制设置、脚本改不了，但**非浏览器客户端可以随便写**。
// 所以这一层防的是"别的站点/别的应用拿你的 site_key 做题"，不是"机器人 vs 真人"；
// CORS 也只决定"浏览器允不允许这个来源读响应"，它从来不阻止请求本身。
func (s *Server) checkOrigin(r *http.Request, site *store.Site) originCheck {
	raw := strings.TrimSpace(r.Header.Get("Origin"))
	if raw == "" {
		return originCheck{Allowed: site.Policy.AllowMissingOrigin, Reason: "missing-origin"}
	}
	norm, err := store.NormalizeOrigin(raw)
	if err != nil {
		return originCheck{Reason: "malformed-origin"}
	}
	ok, err := s.db.OriginAllowed(r.Context(), site.ID, norm)
	if err != nil || !ok {
		return originCheck{Canonical: norm, Reason: "origin-not-allowed"}
	}
	return originCheck{Canonical: norm, Allowed: true, CORS: true, Reason: "ok"}
}

// applyCORS 只在来源被允许时挂头，并且始终带 Vary: Origin——
// 少了 Vary，缓存会把"一个允许的来源"的响应喂给"一个被拒绝的来源"。
func applyCORS(w http.ResponseWriter, o originCheck) {
	if !o.CORS {
		return
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", o.Canonical) // 精确回显已验证的来源，永不用 *
	h.Add("Vary", "Origin")
	h.Set("Access-Control-Allow-Credentials", "false")
}

// handlePreflight 只做预检应答：不签发 challenge、不消费 token、不写任何库行。
func (s *Server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Allow", "OPTIONS, POST")
	// 顺序有讲究：先按来源地址限流与形态检查，再去查库解析站点。
	// 反过来的话，每个匿名 OPTIONS 都会先打一次 SQL——1 vCPU 机器上这是个便宜的放大面。
	if s.preflightRejected(w, r) {
		return
	}
	oc := s.preflightOrigin(r)
	if !oc.Allowed {
		// 来源不在白名单：不给任何 Allow-* 头，让浏览器判定预检失败。
		w.WriteHeader(http.StatusForbidden)
		return
	}
	applyCORS(w, oc)
	h := w.Header()
	h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type")
	h.Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

// preflightOrigin 用 site_key 解析来源是否被允许。预检不落库，所以未知/停用站点一律不给 CORS 头。
func (s *Server) preflightOrigin(r *http.Request) originCheck {
	siteKey := r.URL.Query().Get("site_key")
	if siteKey == "" {
		siteKey = r.Header.Get("X-Epocha-Site-Key")
	}
	if siteKey == "" || len(siteKey) > maxFieldLen {
		return originCheck{}
	}
	site, err := s.db.SiteByKey(r.Context(), siteKey)
	if err != nil || !site.Enabled {
		return originCheck{}
	}
	return s.checkOrigin(r, site)
}

// preflightRejected 对预检做限流与形态检查；返回 true 表示已写响应、调用方必须停手。
func (s *Server) preflightRejected(w http.ResponseWriter, r *http.Request) bool {
	if !s.allowGlobalIP(r, w) {
		return true
	}
	if am := r.Header.Get("Access-Control-Request-Method"); am != "" && !strings.EqualFold(am, "POST") {
		w.WriteHeader(http.StatusForbidden)
		return true
	}
	if ah := r.Header.Get("Access-Control-Request-Headers"); ah != "" &&
		!strings.EqualFold(strings.TrimSpace(ah), "content-type") {
		w.WriteHeader(http.StatusForbidden)
		return true
	}
	return false
}

// --- 公开端点 ---

// handleChallenge 签发一次 PoW 挑战。阶段 2 叠加站点策略与来源裁决；仍然没有题目层。
func (s *Server) handleChallenge(w http.ResponseWriter, r *http.Request) {
	var req challengeRequest
	if err := decodeJSON(w, r, maxBodyPublic, &req); err != nil {
		s.reject(w, CodeInvalidRequest)
		return
	}
	if !fieldOK(req.SiteKey, maxFieldLen) || len(req.Lang) > 35 {
		s.reject(w, CodeInvalidRequest)
		return
	}
	// 站点标识一致性检查排在**限流与任何库操作之前**：它只是字符串比较，
	// 既不该给歧义请求建限流条目，更不该在库里留下半行痕迹。
	declared, conflict := clientSiteKey(r)
	if !siteKeyConsistent(declared, conflict, req.SiteKey) {
		s.log.Info("site_key 声明与请求体不一致", "reason", "site-key-conflict")
		s.reject(w, CodeInvalidRequest)
		return
	}
	// 第一道只按来源地址限流：此时还不知道站点，若按调用方自报的 site_key 建条目，
	// 随便编造 site_key 就能污染站点维度的限流状态。
	if !s.allowGlobalIP(r, w) {
		return
	}
	ctx := r.Context()

	site, err := s.db.SiteByKey(ctx, req.SiteKey)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, "查站点", err)
			return
		}
		s.reject(w, CodeInvalidRequest)
		return
	}
	// 停用的站点与不存在的站点给出同一个响应：不透露哪一层失败（红线 8）。
	if !site.Enabled {
		s.reject(w, CodeInvalidRequest)
		return
	}
	oc := s.checkOrigin(r, site)
	if !oc.Allowed {
		s.log.Info("来源被拒", "site_id", site.ID, "reason", oc.Reason)
		s.reject(w, CodeInvalidRequest)
		return
	}
	applyCORS(w, oc)
	if !s.allowSiteDims(r, site, w) {
		return
	}

	if req.Session != "" && !fieldOK(req.Session, 64) {
		s.reject(w, CodeInvalidRequest)
		return
	}
	device := dispatch.DeviceDesktop
	noMotion := false
	if req.Device != nil {
		switch req.Device.Pointer {
		case "", "fine":
		case "coarse":
			device = dispatch.DeviceTouch
		default:
			s.reject(w, CodeInvalidRequest)
			return
		}
		noMotion = req.Device.ReducedMotion
	}

	// 风险调度只使用进程内可观测的弱信号；它决定"下一次出什么题"，不改变当前判定。
	// pow_only 站点不进入调度器：它没有题型，也不该因为"池为空"被误判成缺题。
	var (
		bits        = int(site.PowBits)
		difficulty  = site.Policy.Difficulty
		selectedID  string
		selectedGen generator.Generator
	)
	now := s.now().UTC()
	if site.Policy.VerifyMode == store.ModePowQuestion {
		riskKey := "site:" + strconv.FormatInt(site.ID, 10) + ":ip:" + risk.Key(s.src.ClientIP(r))
		if req.Session != "" {
			riskKey = "site:" + strconv.FormatInt(site.ID, 10) + ":sess:" + req.Session
		}
		riskScore := 0
		var penalty risk.RefreshPenalty
		// 没有 session 的调用方按旧客户端处理：仍受限流/来源策略约束，但不启用
		// "连续刷新"难度地板。Widget 总会带 session；这是兼容边界，不是信任提升。
		if req.Session != "" && s.tracker != nil {
			riskScore, penalty = s.tracker.Assess(riskKey, 0)
		}
		if penalty.Cooldown > 0 {
			s.rateLimited(w, penalty.Cooldown)
			return
		}
		band := dispatch.BandForScore(riskScore)
		if req.Accessibility && (band == dispatch.BandHigh || band == dispatch.BandCritical) {
			// 高风险不开放无障碍降维；先拒绝，不消耗 24h 名额。
			s.challengeUnavailable(w)
			return
		}
		difficulty = site.Policy.Difficulty + penalty.FloorDelta
		if difficulty > store.MaxDifficulty {
			difficulty = store.MaxDifficulty
		}

		var configured []dispatch.Candidate
		catalog := dispatch.Catalog()
		for _, id := range site.Policy.GeneratorIDs() {
			if _, err := s.gens.Lookup(id); err != nil {
				// 池里有一个未登记 ID：跳过它，但不把整站静默降级成"随便挑一个"。
				continue
			}
			c, ok := catalog[id]
			if !ok {
				c = dispatch.FallbackCandidate(id)
			}
			configured = append(configured, c)
		}
		if req.Accessibility {
			// 先确认池内真的配置了 accessible 候选；没有就 503，不消耗 24h 名额。
			hasAccessible := false
			for _, c := range configured {
				if c.Accessible {
					hasAccessible = true
					break
				}
			}
			if !hasAccessible {
				s.challengeUnavailable(w)
				return
			}
			// 只读预检保持冷却时的 429；真正并发判定仍在签发事务内。
			retry, err := s.db.AccessibilityRetryAfter(ctx, site.ID, risk.Key(s.src.ClientIP(r)), now)
			if err != nil {
				s.internalError(w, r, "无障碍额度检查", err)
				return
			}
			if retry > 0 {
				s.rateLimited(w, retry)
				return
			}
		}
		var candidates []dispatch.Candidate
		// 预检的寿命边界必须与领取事务一致：题目要活到 challenge 的 ExpiresAt。
		// 用 now 会把"还活着但撑不到 challenge 过期"的短命题算进候选——调度器选中它后
		// 领取必然失败，明明同池有可领的长命题却给调用方 503（ADR-034 §6）。
		needAfter := now.Add(site.Policy.ChallengeTTL)
		for _, c := range configured {
			g, err := s.gens.Lookup(c.ID)
			if err != nil {
				continue
			}
			n, err := s.db.ReadyCount(ctx, store.PoolSpec{GeneratorID: c.ID, Version: g.Version(), Difficulty: difficulty}, needAfter)
			if err == nil && n > 0 {
				candidates = append(candidates, c)
			}
		}
		if len(candidates) == 0 || s.scheduler == nil {
			s.challengeUnavailable(w)
			return
		}
		pick, err := s.scheduler.Pick(candidates, dispatch.Request{
			Band: band, Device: device, Accessibility: req.Accessibility, NoMotion: noMotion,
		})
		if err != nil {
			s.challengeUnavailable(w)
			return
		}
		g, err := s.gens.Lookup(pick.ID)
		if err != nil {
			s.challengeUnavailable(w)
			return
		}
		selectedID, selectedGen = pick.ID, g
		bits = int(site.PowBits) + penalty.PoWDelta
		if bits > pow.MaxBits {
			bits = pow.MaxBits
		}
	}

	params, err := pow.NewParams(uint8(bits))
	if err != nil {
		s.internalError(w, r, "生成 PoW 参数", err)
		return
	}
	id, err := store.NewID()
	if err != nil {
		s.internalError(w, r, "生成 challenge id", err)
		return
	}
	ch := &store.Challenge{
		ID: id, SiteID: site.ID, Origin: oc.Canonical,
		PowSeed: params.Seed, PowBits: params.Bits,
		CreatedAt: now, ExpiresAt: now.Add(site.Policy.ChallengeTTL),
	}

	// 模式来自**站点配置**（权威），不是客户端可选参数：请求体里没有 mode 字段，
	// 也没有任何方式让客户端把 pow_question 降级成 pow_only。
	var spec *store.PoolSpec
	if site.Policy.VerifyMode == store.ModePowQuestion {
		spec = &store.PoolSpec{
			GeneratorID: selectedID, Version: selectedGen.Version(),
			Difficulty: difficulty,
		}
		ch.Mode = site.Policy.VerifyMode
		ch.GeneratorID = selectedID
	} else {
		ch.Mode = store.ModePowOnly
	}

	seedB64 := base64.RawURLEncoding.EncodeToString(params.Seed)
	var prepared []byte
	accessibilitySource := ""
	if req.Accessibility && spec != nil {
		accessibilitySource = risk.Key(s.src.ClientIP(r))
	}
	var invalidQuestionID string
	_, err = s.db.IssueChallengePrepared(ctx, ch, s.caps, spec, accessibilitySource, func(q *store.PoolQuestion) error {
		tok, err := s.signer.Sign(token.PurposeChallenge, token.Claims{
			Sid: site.ID, Svid: site.Version, CID: id,
			Exp: ch.ExpiresAt.Unix(), Bits: params.Bits, Seed: seedB64,
			Mode: ch.Mode, Gen: ch.GeneratorID, Sess: req.Session,
		}, now)
		if err != nil {
			return err
		}
		out := challengeResponse{
			Token: tok, ExpiresIn: int64(site.Policy.ChallengeTTL / time.Second),
			Pow: powPayload{Algo: params.Algo, Seed: seedB64, Bits: params.Bits}, Mode: ch.Mode,
		}
		if q != nil {
			dto, err := s.questionDTO(q)
			if err != nil {
				invalidQuestionID = q.ID
				return store.ErrInvalidQuestion
			}
			out.Question = dto
		}
		prepared, err = json.Marshal(out)
		return err
	})
	if err != nil {
		if invalidQuestionID != "" {
			if discardErr := s.db.DiscardInvalidReady(ctx, invalidQuestionID); discardErr != nil {
				s.internalError(w, r, "隔离坏题", discardErr)
				return
			}
		}
		var quota *store.AccessibilityQuotaError
		switch {
		case errors.As(err, &quota):
			s.rateLimited(w, quota.RetryAfter)
		case errors.Is(err, store.ErrCapExceeded):
			s.rateLimited(w, time.Second)
		case errors.Is(err, store.ErrPoolEmpty), errors.Is(err, store.ErrInvalidQuestion):
			s.challengeUnavailable(w)
		default:
			s.internalError(w, r, "写入 challenge", err)
		}
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(prepared)
}

// questionDTO 是题面出网的唯一通道：生成器的白名单字段 + 题型标识（+ 图片资产）。
// 池行 id **不**出现在响应里：客户端不需要、也不该有一个可枚举的题目标识；
// 题目与 challenge 的绑定只在服务端（pool.challenge_id）。
//
// 图片走 base64 内联而不是一个 /asset/<id> 端点（DESIGN §10.1、ADR-012）：
// 裸媒体端点会凭空造出一个需要防枚举、防跨 challenge 引用的面，而 3B 的题是一次性的，
// 内联让"图只跟着它那张 challenge 出去"成为结构事实。
//
// 媒体与资产必须**成对一致**：声明 image 却没有图 ⇒ 客户端拿到一道看不见的题；
// 声明 text 却带图 ⇒ 多出一个客户端不会读的字段。两种都是写库侧的事故，
// 这里 fail closed（作废该 challenge），而不是"少张图也照样发出去"。
func (s *Server) questionDTO(q *store.PoolQuestion) (map[string]any, error) {
	g, err := s.gens.Lookup(q.GeneratorID)
	if err != nil {
		return nil, err
	}
	fields, err := g.SanitizePublic([]byte(q.PayloadJSON))
	if err != nil {
		return nil, err
	}
	fields["generator"] = q.GeneratorID
	fields["generator_version"] = q.Version
	if g.Media() == generator.MediaImage {
		if len(q.Asset) == 0 || q.AssetMime != store.MimeTypePNG {
			return nil, errAssetInconsistent
		}
		if len(q.Asset) > store.MaxAssetBytes {
			return nil, errAssetInconsistent
		}
		cfg, err := png.DecodeConfig(bytes.NewReader(q.Asset))
		if err != nil {
			return nil, errAssetInconsistent
		}
		imageWidth, widthOK := fields["image_width"].(int)
		imageHeight, heightOK := fields["image_height"].(int)
		if !widthOK || !heightOK || imageWidth < 1 || imageHeight < 1 {
			return nil, errAssetInconsistent
		}
		wantWidth, wantHeight := imageWidth, imageHeight
		if mode, ok := fields["render_mode"]; ok {
			renderMode, ok := mode.(string)
			if !ok {
				return nil, errAssetInconsistent
			}
			switch renderMode {
			case generator.RenderModeStatic:
			case generator.RenderModeSpriteSheet:
				cols, colsOK := fields["frame_cols"].(int)
				rows, rowsOK := fields["frame_rows"].(int)
				frameCount, countOK := fields["frame_count"].(int)
				frameMS, frameMSOK := fields["frame_ms"].(int)
				frameLoop, loopOK := fields["frame_loop"].(bool)
				if !colsOK || !rowsOK || !countOK || !frameMSOK || !loopOK {
					return nil, errAssetInconsistent
				}
				frames := generator.FrameSpec{
					Mode: renderMode, Count: frameCount, Cols: cols, Rows: rows,
					FrameMS: frameMS, Loop: frameLoop,
				}
				if frames.Validate() != nil {
					return nil, errAssetInconsistent
				}
				wantWidth, wantHeight, err = frames.SheetSize(imageWidth, imageHeight)
				if err != nil {
					return nil, errAssetInconsistent
				}
			default:
				return nil, errAssetInconsistent
			}
		}
		if cfg.Width != wantWidth || cfg.Height != wantHeight {
			return nil, errAssetInconsistent
		}
		fields["media"] = generator.MediaImage
		fields["image_mime"] = q.AssetMime
		fields["image_base64"] = base64.StdEncoding.EncodeToString(q.Asset)
		return fields, nil
	}
	if len(q.Asset) > 0 || q.AssetMime != "" {
		return nil, errAssetInconsistent
	}
	// accessible-choice 是唯一显式的纯文本题型；给客户端一个明确的媒体标识，
	// 而不是让它从 options 是否存在去猜。
	if q.GeneratorID == generator.AccessibleChoiceID {
		fields["media"] = generator.MediaText
	}
	return fields, nil
}

// errAssetInconsistent 是"媒体声明与资产不配对"。它走的是**题面不合法**那条路径：
// 作废这张 challenge 并返回可重试的 503，绝不下发一道看不见题面的题。
var errAssetInconsistent = errors.New("api: 题目媒体与资产不一致")

// challengeUnavailable 是"暂时给不出题"的可重试响应：503 + Retry-After。
// 它与限流分开，因为业务方的处理动作不同（限流要退避更久，缺题只要稍后重试）。
func (s *Server) challengeUnavailable(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "2")
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error-codes": []string{CodeChallengeUnavailable}})
}

// handleRedeem 校验 PoW 解并原子消费 challenge。
func (s *Server) handleRedeem(w http.ResponseWriter, r *http.Request) {
	var req redeemRequest
	if err := decodeJSON(w, r, maxBodyPublic, &req); err != nil {
		s.reject(w, CodeInvalidRequest)
		return
	}
	if !fieldOK(req.Token, 4096) || len(req.PowNonce) == 0 || len(req.PowNonce) > maxPowNonceLen ||
		len(req.Answer) > maxAnswerLen {
		s.reject(w, CodeInvalidRequest)
		return
	}
	if req.Telemetry != nil {
		if err := req.Telemetry.Validate(); err != nil {
			// 遥测畸形是结构错误，不消耗作答机会；它仍不参与判定。
			s.reject(w, CodeInvalidRequest)
			return
		}
	}
	if !s.allowGlobalIP(r, w) {
		return
	}
	ctx := r.Context()
	now := s.now().UTC()

	claims, err := s.signer.Verify(token.PurposeChallenge, req.Token, now)
	if err != nil {
		// 篡改、过期、用途混淆、未知 kid、缺站点版本一律同一结果：不给探测反馈。
		s.rejectChallenge(w, "")
		return
	}
	site, err := s.db.SiteByID(ctx, claims.Sid)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, "查站点", err)
			return
		}
		s.rejectChallenge(w, claims.CID)
		return
	}
	// 站点版本必须与签发时一致：停用（以及停用后再启用）让此前签发的 token 全部失效。
	if !site.Enabled || site.Version != claims.Svid {
		s.log.Info("站点状态使 token 失效", "site_id", site.ID, "cid8", shortCID(claims.CID))
		s.rejectChallenge(w, claims.CID)
		return
	}
	// redeem 的权威站点标识来自**token**（签名覆盖），所以这里的规则是：
	// 客户端在 URL 上声明的 site_key 必须与 token 属于同一个站点。
	// 检查放在任何消费之前：被这条挡下的请求**不得**消耗唯一一次作答机会（有测试钉住）。
	if declared, conflict := clientSiteKey(r); !siteKeyConsistent(declared, conflict, site.SiteKey) {
		s.log.Info("site_key 与 token 站点不一致", "site_id", site.ID, "cid8", shortCID(claims.CID))
		s.reject(w, CodeInvalidRequest)
		return
	}
	if !s.allowSiteDims(r, site, w) {
		return
	}
	oc := s.checkOrigin(r, site)
	if !oc.Allowed {
		s.log.Info("来源被拒", "site_id", site.ID, "reason", oc.Reason)
		s.rejectChallenge(w, claims.CID)
		return
	}
	applyCORS(w, oc)

	ch, err := s.db.Challenge(ctx, claims.CID)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, "读 challenge", err)
			return
		}
		s.rejectChallenge(w, claims.CID)
		return
	}
	// token 与库内记录必须完全一致：这是"签名之外再以库为权威"的纵深防御（DESIGN §6）。
	// 模式与生成器也在这里比对：客户端无法把 pow_question 降级成 pow_only，也无法换题型。
	if ch.SiteID != claims.Sid || ch.PowBits != claims.Bits ||
		ch.Mode != claims.Mode || ch.GeneratorID != claims.Gen ||
		base64.RawURLEncoding.EncodeToString(ch.PowSeed) != claims.Seed {
		s.rejectChallenge(w, ch.ID)
		return
	}
	// 来源必须与签发时绑定的同一个：换个来源重放同一 token 不行。
	if ch.Origin != oc.Canonical {
		s.log.Info("来源与签发不一致", "cid8", shortCID(ch.ID))
		s.rejectChallenge(w, ch.ID)
		return
	}
	if ch.Status != "issued" || !now.Before(ch.ExpiresAt) {
		s.rejectChallenge(w, ch.ID)
		return
	}
	// --- 有效作答尝试的门槛 ---
	// 顺序是刻意的：token → 站点/来源 → 结构（含答案文法）→ PoW → 才进入**会终结状态**的那一步。
	// 畸形答案在这之前就被挡掉，因此它不会消耗掉 pow_question 唯一的作答机会；
	// 而"文法正确但数值错"的答案会终结 challenge（一次机会，ADR-011 #3）。
	var (
		verdict         bool
		requireQuestion = ch.Mode == store.ModePowQuestion
	)
	if requireQuestion {
		if req.Answer == "" {
			s.reject(w, CodeInvalidRequest)
			return
		}
		g, err := s.gens.Lookup(ch.GeneratorID)
		if err != nil {
			s.log.Warn("生成器不可用", "site_id", site.ID, "err", errKind(err))
			s.challengeUnavailable(w)
			return
		}
		// 判分只读 verifier 一个字段：**不**读题面、更不读图片资产。
		// 于是"资产被清理/损坏"在结构上不可能影响判分（有测试钉住这条）。
		verifier, err := s.db.QuestionVerifierForChallenge(ctx, ch.ID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// 绑定丢了：绝不"那就按没题处理"放行，那是把 pow_question 静默降级
				s.log.Error("pow_question 的 challenge 没有绑定题目", "cid8", shortCID(ch.ID))
				s.rejectChallenge(w, ch.ID)
				return
			}
			s.internalError(w, r, "查绑定题目", err)
			return
		}
		canon, okGrammar := g.NormalizeAnswer(req.Answer)
		if !okGrammar {
			// 畸形 ≠ 答错：不终态化（否则攻击者可以用垃圾输入把受害者的机会耗光，
			// 更要紧的是"格式差异"会变成安全判定）
			s.reject(w, CodeInvalidRequest)
			return
		}
		verdict = g.Verify(generator.PrivateVerifier(verifier), canon)
	}

	ok, err := pow.Params{Algo: pow.Algo, Bits: ch.PowBits, Seed: ch.PowSeed}.Verify(req.PowNonce)
	if err != nil {
		s.rejectChallenge(w, ch.ID)
		return
	}
	if !ok {
		// PoW 不对不算作答；pow_only 沿用阶段 1 的"累计 N 次失败作废"
		if _, _, ferr := s.db.FailAttempt(ctx, ch.ID); ferr != nil && !errors.Is(ferr, store.ErrNotFound) {
			s.internalError(w, r, "记录失败尝试", ferr)
			return
		}
		s.rejectChallenge(w, ch.ID)
		return
	}
	if !requireQuestion {
		verdict = true
	}
	res, err := s.db.RedeemAttempt(ctx, ch.ID, now, site.Policy.ResponseTTL, verdict)
	if err != nil {
		if !errors.Is(err, store.ErrNotRedeemable) {
			s.internalError(w, r, "兑换", err)
			return
		}
		s.rejectChallenge(w, ch.ID)
		return
	}
	if s.tracker != nil {
		riskKey := "site:" + strconv.FormatInt(site.ID, 10) + ":ip:" + risk.Key(s.src.ClientIP(r))
		if claims.Sess != "" {
			riskKey = "site:" + strconv.FormatInt(site.ID, 10) + ":sess:" + claims.Sess
		}
		// 遥测只在显式判定已提交后更新风险状态；它不改变 verdict，也不进入响应。
		s.tracker.ObserveRedeem(riskKey, res.Outcome != store.OutcomeWrongAnswer, req.Telemetry)
	}
	if res.Outcome == store.OutcomeWrongAnswer {
		// 答案错：状态**已经提交**为终态。这里返回的必须是"不可兑换"，
		// 而且绝不允许把这条路径改成返回 error 触发回滚——那等于给回重试机会（有回归测试钉住）。
		s.log.Info("答案错误，challenge 已终结", "cid8", shortCID(ch.ID))
		s.rejectChallenge(w, ch.ID)
		return
	}
	resp := res.Response
	tok, err := s.signer.Sign(token.PurposeResponse, token.Claims{
		Sid: resp.SiteID, Svid: site.Version, CID: resp.ChallengeID, RID: resp.ID,
		Exp: resp.ExpiresAt.Unix(),
	}, now)
	if err != nil {
		s.internalError(w, r, "签名", err)
		return
	}
	// 成功结果在事务提交之后才返回（DESIGN D12）。
	writeJSON(w, http.StatusOK, redeemResponse{
		Response:  tok,
		ExpiresIn: int64(site.Policy.ResponseTTL / time.Second),
	})
}

// handleSiteverify 返回 reCAPTCHA 风格 JSON。业务错误同样 HTTP 200（reCAPTCHA 语义）。
// 这是服务端到服务端的接口：**不挂任何 CORS 头**，不让任意页面在浏览器里读验证结果。
func (s *Server) handleSiteverify(w http.ResponseWriter, r *http.Request) {
	form, err := parseVerifyBody(w, r)
	if err != nil {
		verifyFail(w, ErrBadRequest)
		return
	}
	if !s.allowGlobalIP(r, w) {
		return
	}
	ctx := r.Context()
	now := s.now().UTC()

	secret, response := form["secret"], form["response"]
	if secret == "" {
		verifyFail(w, ErrMissingInputSecret)
		return
	}
	if response == "" || len(response) > 4096 {
		verifyFail(w, ErrMissingInputResponse)
		return
	}
	// 先定位站点：secret 不对或已被撤销就绝不触碰任何 token 状态（DESIGN §10.1）。
	site, err := s.db.FindSiteBySecret(ctx, secret, now)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			s.internalError(w, r, "查站点", err)
			return
		}
		verifyFail(w, ErrInvalidInputSecret)
		return
	}
	claims, err := s.signer.Verify(token.PurposeResponse, response, now)
	if err != nil {
		if errors.Is(err, token.ErrExpired) {
			verifyFail(w, ErrTimeoutOrDuplicate)
			return
		}
		verifyFail(w, ErrInvalidInputResponse)
		return
	}
	// 站点裁决（enabled + 版本）与消费 UPDATE 在同一事务内（ADR 线性化点 = 消费事务）：
	// 停用/版本提升先提交 ⇒ 本次验证拒绝且 response 保持未用；消费先提交 ⇒ 成功保留。
	// ErrSiteDisabled 与"版本不匹配"统一映射 invalid-input-response，不向调用方区分原因（红线 8）。
	v, err := s.db.ConsumeResponseSiteChecked(ctx, claims.RID, site.ID, claims.Svid, now)
	if err != nil {
		if errors.Is(err, store.ErrConsumed) || errors.Is(err, store.ErrNotFound) {
			// 已使用、不存在或已过期：与 reCAPTCHA 的 timeout-or-duplicate 语义一致。
			verifyFail(w, ErrTimeoutOrDuplicate)
			return
		}
		// 停用/版本变动与跨站点归属错误都统一 invalid-input-response，不区分原因（红线 8）。
		if errors.Is(err, store.ErrSiteDisabled) || errors.Is(err, store.ErrSiteOwnership) {
			verifyFail(w, ErrInvalidInputResponse)
			return
		}
		s.internalError(w, r, "消费 response", err)
		return
	}
	// 消费成功后才返回成功；异常时返回内部错误而不是假成功。
	writeVerifySuccess(w, v)
}

// writeVerifySuccess 的 hostname 取自**签发时绑定在账本行里的规范化 Origin**，
// 不采信调用方在 /siteverify 自报的 hostname（DESIGN §10.3）。
// 保证范围必须说清楚：它表示"这个 challenge 在这个允许来源上签发"，
// 而 Origin 头对非浏览器客户端可伪造——所以它不是机器人身份证明，也没有任何
// 硬件级 / 浏览器证明 / 可信远程证明的含义，只是服务端记录并签名绑定过的一个输入值。
// 直接 API 调用（无 Origin 且被站点放行）时该字段整体缺省，而不是回显调用方给的值。
func writeVerifySuccess(w http.ResponseWriter, v *store.Verification) {
	body := map[string]any{
		"success":      true,
		"challenge_ts": v.ChallengeCreatedAt.UTC().Format(time.RFC3339),
	}
	if host := store.OriginHost(v.Origin); host != "" && len(host) <= maxHostnameLen {
		body["hostname"] = host
	}
	writeJSON(w, http.StatusOK, body)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	// 只回答"进程在跑"：不暴露版本、路径、站点数、依赖健康，也不代表验证语义可用。
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReadyz 回答"现在能不能接流量"，判据是存储层的一次有界自检（store.Ready）。
//
// 与 /healthz 的分工是刻意的：/healthz 只证明进程存在，而"每次验证都失败"的实例
// 也完全可能同时是活着的（WAL 库读得动但目录只读、磁盘满、迁移没跑完）。
// 容器编排需要后者才能不把故障实例当成健康。
//
// 三条边界（阶段 5A 要求）：
//   - **不分配题目、不消费 token**：探针本身不得改变任何安全关键状态（有测试对账行数）；
//   - **有界**：store.Ready 自带 2s 上限，且沿用请求 ctx，探针慢成故障源就直接判不就绪；
//   - **不外泄**：只回 status 一词，不写路径、SQL、驱动文本、站点数、题池水位。
//     题池水位低是容量信号（业务侧本来就会给 503 + 不降级），不是未就绪。
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.db.Ready(r.Context()); err != nil {
		// 与 /api/* 的其它 5xx 一样：日志记类别，响应不记原因（红线 8）。
		s.log.Warn("未就绪", "err", errKind(err))
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"status": "unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

// --- 内部辅助 ---

func (s *Server) reject(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusBadRequest, map[string]any{"error-codes": []string{code}})
}

func (s *Server) rejectChallenge(w http.ResponseWriter, cid string) {
	// cid 前 8 字符可用于关联日志（不是密钥，也不足以定位 token）。
	if c := shortCID(cid); c != "" {
		s.log.Info("challenge 被拒", "cid8", c)
	}
	writeJSON(w, http.StatusBadRequest, map[string]any{"error-codes": []string{CodeInvalidChallenge}})
}

func shortCID(cid string) string {
	if len(cid) > 8 {
		return cid[:8]
	}
	return cid
}

// internalError 按端点选择错误码族：/siteverify 是 reCAPTCHA 契约面，服务端故障只能用
// 唯一扩展码 internal-error 随 5xx 返回，业务方据此判断"验证未完成"而不是"验证失败"
// （DESIGN §10.1）。响应保持 {success:false, error-codes:[...]} 的文档形状，让调用方的
// 解析器不必为 5xx 特例处理缺失字段。/api/* 用自有码集，不引入该码。
func (s *Server) internalError(w http.ResponseWriter, r *http.Request, what string, err error) {
	s.log.Error("内部错误", "where", what, "err", errKind(err))
	if r.URL.Path == "/siteverify" {
		writeJSON(w, http.StatusInternalServerError,
			map[string]any{"success": false, "error-codes": []string{ErrInternalError}})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]any{"error-codes": []string{CodeInvalidRequest}})
}

// errKind 只保留错误类别，避免把库/驱动细节带进日志之外的任何地方。
func errKind(err error) string {
	switch {
	case err == nil:
		return "nil"
	case errors.Is(err, store.ErrNotFound):
		return "not-found"
	case errors.Is(err, store.ErrNotRedeemable):
		return "not-redeemable"
	case errors.Is(err, store.ErrConsumed):
		return "consumed"
	case errors.Is(err, store.ErrBadConfig):
		return "bad-config"
	case errors.Is(err, store.ErrCapExceeded):
		return "cap-exceeded"
	case errors.Is(err, store.ErrConflict):
		return "conflict"
	case errors.Is(err, store.ErrPoolEmpty):
		return "pool-empty"
	case errors.Is(err, generator.ErrUnknownGenerator):
		return "unknown-generator"
	case errors.Is(err, store.ErrNotReady):
		return "not-ready"
	default:
		return "unspecified"
	}
}

// allowGlobalIP 是来源地址维度的第一道闸门（站点未知时也只走这一道）。
func (s *Server) allowGlobalIP(r *http.Request, w http.ResponseWriter) bool {
	key := "ip:" + risk.Key(s.src.ClientIP(r))
	if !s.ipLimit.Allow(key) {
		s.rateLimited(w, s.ipLimit.RetryAfter(key))
		return false
	}
	return true
}

// allowSiteDims 是站点定位后的两维计数：站点整体速率，以及"该站点 + 该来源地址"速率。
// key 一律取库中的站点 id，绝不取调用方自报的 site_key 字符串（阶段 1 复核发现的污染面）。
func (s *Server) allowSiteDims(r *http.Request, site *store.Site, w http.ResponseWriter) bool {
	siteDim := "site:" + strconv.FormatInt(site.ID, 10)
	perPair := "ips:" + strconv.FormatInt(site.ID, 10) + ":" + risk.Key(s.src.ClientIP(r))
	if !s.siteLimit.AllowN(siteDim, site.Policy.RateSiteRPM) {
		s.rateLimited(w, s.siteLimit.RetryAfter(siteDim))
		return false
	}
	if !s.ipLimit.AllowN(perPair, site.Policy.RateIPRPM) {
		s.rateLimited(w, s.ipLimit.RetryAfter(perPair))
		return false
	}
	return true
}

func (s *Server) rateLimited(w http.ResponseWriter, retry time.Duration) {
	if retry > 0 {
		w.Header().Set("Retry-After", retryAfterHeader(retry))
	}
	writeJSON(w, http.StatusTooManyRequests, map[string]any{"error-codes": []string{CodeRateLimited}})
}

// parseVerifyBody 同时支持 form 与 JSON；只读白名单字段，其余忽略（reCAPTCHA 客户端会带额外字段）。
// 注意：这里读到的 hostname/remoteip 都不参与安全判定（DESIGN §10.3、§12）。
func parseVerifyBody(w http.ResponseWriter, r *http.Request) (map[string]string, error) {
	defer r.Body.Close()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyVerify))
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var req struct {
			Secret   string `json:"secret"`
			Response string `json:"response"`
			RemoteIP string `json:"remoteip"`
			Hostname string `json:"hostname"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		out["secret"], out["response"] = req.Secret, req.Response
		out["remoteip"], out["hostname"] = req.RemoteIP, req.Hostname
		return out, nil
	}
	vals, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	for _, k := range []string{"secret", "response", "remoteip", "hostname"} {
		out[k] = vals.Get(k)
	}
	return out, nil
}

// verifyFail 只回 success 与 error-codes 两个键：不携带 challenge_ts/hostname，
// 避免"失败也回完整结构"成为探测面（DESIGN §10.1）。
func verifyFail(w http.ResponseWriter, code string) {
	writeJSON(w, http.StatusOK, map[string]any{"success": false, "error-codes": []string{code}})
}
