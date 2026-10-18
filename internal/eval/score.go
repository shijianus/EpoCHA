package eval

// 评分器：**不依赖任何 OCR 工具**的那一半，因此可以先被完全测试。
//
// 它做的事情很窄但必须准确：把"外部工具的输出"与"ground truth"逐条对齐计数。
// 三个刻意的设计决定：
//
//  1. 通过率有**两个分母**，而且都写进结果里。只报一个数就能把结论做出来：
//     剔除工具失败与缺测后是"工具在能跑的样本上有多准"（ScoredAccuracy），
//     把失败与缺测都算不通过是"这条流水线整体交付出多少正确答案"（StrictAccuracy）。
//     报告里必须同时给，否则一次大面积 OCR 崩溃会被包装成"题目很难"。
//  2. 归一化由调用方注入，且**必须是生产判分用的那个实现**："什么算接受"是题型策略，
//     不是评分器策略。评测里自己写一份更宽容的归一化（典型例子：删掉 OCR 夹进来的空格）
//     会测出一个服务端根本不会给的通过率，而这个数字唯一的用途就是估计服务端接受率。
//     畸形输入（生产会 400 的那类）单独计入 Metrics.Malformed，不算 Exact。
//  3. 对齐失败（预测里有 ground truth 里没有的文件、重复预测）是**错误**而不是忽略：
//     一条静默丢样本的流水线会让样本量悄悄变小，而样本量正是这些数字唯一的意思。

import (
	"fmt"
	"sort"
)

// Sample 是一条 ground truth：文件名 → 期望答案。
type Sample struct {
	File       string `json:"file"`
	Answer     string `json:"answer"`
	Generator  string `json:"generator"`
	Version    string `json:"version"`
	Difficulty int    `json:"difficulty"`
	Seed       string `json:"seed,omitempty"` // 复现信息；只落本地评测目录
}

// Prediction 是外部工具对一个文件的输出。
// Err 非空表示**工具侧失败**（进程没起来、超时、非零退出、输出为空）：
// 它不是"答错"，两者分开计数，否则一次 OCR 崩溃会被读成"题目抗机器能力强"。
type Prediction struct {
	File string `json:"file"`
	Text string `json:"text"`
	MS   int64  `json:"ms"`
	Err  string `json:"error,omitempty"`
}

// Metrics 是一次评测的全部计数与耗时分布。
type Metrics struct {
	Total        int // ground truth 条数
	Predicted    int // 有输出的条数（含失败）
	ToolFailures int // Err 非空
	Missing      int // ground truth 里有、预测里没有
	Unknown      int // 预测里有、ground truth 里没有（不参与任何分母）
	Exact        int // 归一化后完全一致
	Mismatched   int // 有输出、非失败、格式合法但不等于 ground truth
	Malformed    int // 有输出但**服务端会当畸形输入拒掉**（400），连"答错"都算不上

	// StrictAccuracy = Exact / Total；ScoredAccuracy = Exact / (Predicted − ToolFailures − Unknown)。
	// 分母为 0 时是 0（不是 1，也不是 NaN）。
	StrictAccuracy float64
	ScoredAccuracy float64

	// CharAccuracy：位置对齐的单字符正确率，只统计**长度相等**的那些预测。
	// 它对"只差一个字符"这种失败形状敏感；整串匹配会把 6 个字符错 1 个记成 0 分。
	CharAccuracy float64

	// 每张处理耗时：只统计非失败预测（失败的耗时是"跑到超时"，混进来会抬高均值）。
	MSCount  int
	MSSum    int64
	MSMin    int64
	MSMean   int64
	MSMedian int64
	MSMax    int64
}

// Normalizer 是"什么算一个可比较的答案串"的注入点。**必须**与生产判分用同一个实现：
// 评测里如果自带一份更宽容的归一化（例如把 OCR 夹进来的空格删掉），测出来的通过率
// 会比服务端真正接受的通过率高，而这条流水线唯一的目的就是估计后者。
// 第二个返回值 false = **畸形输入**：服务端会直接 400 invalid-request，连"答错"都算不上。
type Normalizer func(string) (string, bool)

// Score 把预测与 ground truth 对齐计数。normalize 必须是幂等纯函数；nil = 逐字节比较（一律合法）。
func Score(ground []Sample, pred []Prediction, normalize Normalizer) (Metrics, error) {
	if normalize == nil {
		normalize = func(s string) (string, bool) { return s, true }
	}
	truth := make(map[string]string, len(ground))
	for _, s := range ground {
		if s.File == "" {
			return Metrics{}, fmt.Errorf("eval: ground truth 有空文件名")
		}
		if _, dup := truth[s.File]; dup {
			return Metrics{}, fmt.Errorf("eval: ground truth 文件重复 %q", s.File)
		}
		want, ok := normalize(s.Answer)
		if !ok {
			// ground truth 自己畸形说明**样本集**坏了（生成器与评测集不同步），必须炸在这里。
			return Metrics{}, fmt.Errorf("eval: ground truth 答案畸形 %q=%q", s.File, s.Answer)
		}
		truth[s.File] = want
	}
	m := Metrics{Total: len(truth)}
	seen := make(map[string]bool, len(pred))
	times := make([]int64, 0, len(pred))
	var charTotal, charHits int

	for _, p := range pred {
		want, ok := truth[p.File]
		if !ok {
			m.Unknown++
			continue
		}
		if seen[p.File] {
			return Metrics{}, fmt.Errorf("eval: 同一文件多条预测 %q", p.File)
		}
		seen[p.File] = true
		m.Predicted++
		if p.Err != "" {
			m.ToolFailures++
			continue
		}
		times = append(times, p.MS)
		got, valid := normalize(p.Text)
		switch {
		case !valid:
			// 服务端在这种情况下连判分都不做（400），所以它绝不是"答对"。
			m.Malformed++
		case got == want:
			m.Exact++
		default:
			m.Mismatched++
		}
		if valid && len(want) == len(got) && len(want) > 0 {
			charTotal += len(want)
			for i := 0; i < len(want); i++ {
				if want[i] == got[i] {
					charHits++
				}
			}
		}
	}
	m.Missing = m.Total - len(seen)

	if m.Total > 0 {
		m.StrictAccuracy = float64(m.Exact) / float64(m.Total)
	}
	if scored := m.Predicted - m.ToolFailures - m.Unknown; scored > 0 {
		m.ScoredAccuracy = float64(m.Exact) / float64(scored)
	}
	if charTotal > 0 {
		m.CharAccuracy = float64(charHits) / float64(charTotal)
	}
	summarizeTimes(&m, times)
	return m, nil
}

func summarizeTimes(m *Metrics, times []int64) {
	m.MSCount = len(times)
	if len(times) == 0 {
		return
	}
	sort.Slice(times, func(i, j int) bool { return times[i] < times[j] })
	m.MSMin, m.MSMax = times[0], times[len(times)-1]
	for _, v := range times {
		m.MSSum += v
	}
	m.MSMedian = times[len(times)/2]
	if len(times)%2 == 0 {
		m.MSMedian = (times[len(times)/2-1] + times[len(times)/2]) / 2
	}
	m.MSMean = m.MSSum / int64(len(times))
}
