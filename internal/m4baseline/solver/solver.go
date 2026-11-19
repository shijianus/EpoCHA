// Package solver 是 M4 Qualification Gate B 的**开发期**攻击基线。
//
// 这个包刻意不 import 仓库里的任何其它包：它的全部输入只有一个 JSON——即浏览器在
// /api/challenge 响应里拿到的公开 DTO（SanitizePublic 白名单字段 + base64 PNG）——
// 以及从该 DTO 解码出的像素。它拿不到 verifier、数据库行、生成器私有 seed 或管理面内容。
//
// 每个 Solve 都是确定性的：相同 DTO 必得相同结果；失败分两类：
//   - ParseFailed：公开 DTO 解不开（不是攻击难度，而是攻击器输入面坏了）；
//   - GaveUp：解析成功但没有找到唯一解（攻击器主动放弃，不能记成"题型抗机器"）。
package solver

// Outcome 是一次攻击的结果。Answer 必须是生产 NormalizeAnswer 能接受的规范形式；
// 它只交给 harness 的判分步骤，不参与任何生产路径。
type Outcome struct {
	Answer      string
	ParseFailed bool
	GaveUp      bool
	Note        string
}

// Solve 按 generator ID 分派。未知 ID 是 ParseFailed（攻击器没有覆盖它）。
func Solve(generatorID string, publicJSON []byte) Outcome {
	switch generatorID {
	case "tile-rotation-v1":
		return solveTileRotation(publicJSON)
	case "symbol-path-v1":
		return solveSymbolPath(publicJSON)
	case "crowd-find-v1":
		return solveCrowdFind(publicJSON)
	case "visual-rank-v1":
		return solveVisualRank(publicJSON)
	case "progressive-target-v1":
		return solveProgressiveTarget(publicJSON)
	case "shell-track-v1":
		return solveShellTrack(publicJSON)
	case "silhouette-assembly-v1":
		return solveSilhouetteAssembly(publicJSON)
	default:
		return Outcome{ParseFailed: true, Note: "unknown generator"}
	}
}

func parseFailure(note string) Outcome {
	return Outcome{ParseFailed: true, Note: note}
}

func giveUp(note string) Outcome {
	return Outcome{GaveUp: true, Note: note}
}

func answer(s string) Outcome {
	return Outcome{Answer: s}
}
