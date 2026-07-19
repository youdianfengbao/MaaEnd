package visitfriends

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strings"

	maa "github.com/MaaXYZ/maa-framework-go/v4"
	"github.com/rs/zerolog/log"
)

const (
	selectFriendRecognitionName            = "VisitFriendsSelectFriendRecognition"
	selectFriendCandidateNode              = "VisitFriendsRecognitionItemWithName"
	selectFriendNameOCRNode                = "VisitFriendsRecognitionItemNameByEnterButton"
	selectFriendClueExchangeNode           = "VisitFriendsRecognitionItemClueExchangeByEnterButton"
	selectFriendAttachVisited              = "visited"
	selectFriendAttachClueExchangeExhausted = "clue_exchange_exhausted"
)

// normalizeFriendName 清洗 OCR 识别到的好友名，去除尾部噪声：
// 若包含右括号（) 或 ）），保留到右括号为止；
// 否则若包含 #，保留到 # 后的 4 个字符为止。
func normalizeFriendName(name string) string {
	runes := []rune(name)
	for i, r := range runes {
		if r == ')' || r == '）' {
			return string(runes[:i+1])
		}
	}
	for i, r := range runes {
		if r == '#' {
			end := i + 1 + 4
			if end > len(runes) {
				end = len(runes)
			}
			return string(runes[:end])
		}
	}
	return name
}

// VisitFriendsSelectFriendRecognition 参考 DailyEventGoToRecognition：
// 读取 attach.visited，排除已点好友后识别列表项，返回进船按钮框供 Pipeline Click。
type VisitFriendsSelectFriendRecognition struct{}

var _ maa.CustomRecognitionRunner = &VisitFriendsSelectFriendRecognition{}

type selectFriendParam struct {
	OnlyRemarkFriends bool `json:"only_remark_friends"`
}

type selectFriendDetail struct {
	NameText  string `json:"name_text"`
	ButtonBox []int  `json:"button_box"`
}

func (r *VisitFriendsSelectFriendRecognition) Run(ctx *maa.Context, arg *maa.CustomRecognitionArg) (*maa.CustomRecognitionResult, bool) {
	if ctx == nil || arg == nil {
		log.Error().Str("component", selectFriendRecognitionName).Msg("nil context or arg")
		return nil, false
	}

	nodeName := strings.TrimSpace(arg.CurrentTaskName)
	if nodeName == "" {
		log.Error().Str("component", selectFriendRecognitionName).Msg("current task name is empty")
		return nil, false
	}

	var params selectFriendParam
	if raw := strings.TrimSpace(arg.CustomRecognitionParam); raw != "" {
		if err := json.Unmarshal([]byte(raw), &params); err != nil {
			log.Error().Err(err).Str("component", selectFriendRecognitionName).Msg("failed to parse custom_recognition_param")
			return nil, false
		}
	}

	visited, err := loadSelectFriendVisited(ctx, nodeName)
	if err != nil {
		log.Error().Err(err).Str("component", selectFriendRecognitionName).Str("node", nodeName).Msg("load attach.visited failed")
		return nil, false
	}

	// 与 DailyEventGoTo 一样覆盖 OCR expected，减少已访问命中；
	// 覆盖 OCR expected 排除已访问；按钮与名字按纵向最近邻配对，不要求数量一致。
	expected := buildSelectFriendExpected(visited)
	if err := ctx.OverridePipeline(map[string]any{
		selectFriendNameOCRNode: map[string]any{
			"expected": []string{expected},
		},
	}); err != nil {
		log.Error().Err(err).Str("component", selectFriendRecognitionName).Msg("override name OCR expected failed")
		return nil, false
	}

	detail, err := ctx.RunRecognition(selectFriendCandidateNode, arg.Img)
	if err != nil {
		log.Error().Err(err).Str("component", selectFriendRecognitionName).Str("node", selectFriendCandidateNode).Msg("RunRecognition failed")
		return nil, false
	}
	if detail == nil || !detail.Hit || detail.CombinedResult == nil || len(detail.CombinedResult) < 3 {
		log.Info().Str("component", selectFriendRecognitionName).Strs("visited", visited).Msg("no friend candidate")
		return nil, false
	}

	buttonHits, nameHits, ok := parseSelectFriendCombinedHits(detail)
	if !ok {
		return nil, false
	}
	if len(buttonHits) == 0 || len(nameHits) == 0 {
		log.Info().
			Str("component", selectFriendRecognitionName).
			Int("buttons", len(buttonHits)).
			Int("names", len(nameHits)).
			Msg("empty button or name hits")
		return nil, false
	}

	// 收集所有有效候选（不立即 break）
	var candidates []selectFriendDetail
	for i := range nameHits {
		rawName := strings.TrimSpace(nameHits[i].Text)
		if rawName == "" {
			continue
		}
		if params.OnlyRemarkFriends && !friendNameHasRemark(rawName) {
			log.Debug().Str("component", selectFriendRecognitionName).Str("name", rawName).Msg("no remark, skip")
			continue
		}

		name := normalizeFriendName(rawName)
		if selectFriendVisitedContains(visited, name) {
			log.Debug().Str("component", selectFriendRecognitionName).Str("name", name).Msg("already visited, skip")
			continue
		}

		buttonBox, paired := nearestButtonBoxByVertical(buttonHits, nameHits[i].Box)
		if !paired {
			log.Warn().Str("component", selectFriendRecognitionName).Str("name", name).Msg("no enter button near name")
			continue
		}

		candidates = append(candidates, selectFriendDetail{
			NameText:  name,
			ButtonBox: buttonBox,
		})
	}
	if len(candidates) == 0 {
		log.Info().Str("component", selectFriendRecognitionName).Strs("visited", visited).Msg("no unvisited friend on screen")
		return nil, false
	}

	// 读取翻页穷尽标志（由 ScrollFinish 的 PipelineOverrideAction 设置）
	exhausted := loadClueExchangeExhausted(ctx, nodeName)

	var selected *selectFriendDetail
	if !exhausted {
		// 情报交流优先级：逐个候选检测情报交流图标
		for i := range candidates {
			override := map[string]any{
				selectFriendClueExchangeNode: map[string]any{
					"roi": candidates[i].ButtonBox,
				},
			}
			detail, err := ctx.RunRecognition(selectFriendClueExchangeNode, arg.Img, override)
			if err == nil && detail != nil && detail.Hit {
				selected = &candidates[i]
				log.Debug().
					Str("component", selectFriendRecognitionName).
					Str("name", candidates[i].NameText).
					Msg("found clue exchange friend")
				break
			}
		}
		if selected == nil {
			// 当前页无情报交流好友，触发翻页
			log.Info().
				Str("component", selectFriendRecognitionName).
				Int("candidates", len(candidates)).
				Msg("no clue exchange friend on current page, need scroll")
			return nil, false
		}
	}

	// 回落：已穷尽时取第一个候选
	if selected == nil {
		selected = &candidates[0]
		log.Debug().
			Str("component", selectFriendRecognitionName).
			Str("name", selected.NameText).
			Msg("fallback: clue exchange exhausted, pick first candidate")
	}

	newVisited := append(append([]string{}, visited...), selected.NameText)
	// 保存 visited 并清除 clue_exchange_exhausted，避免死循环
	if err := ctx.OverridePipeline(map[string]any{
		nodeName: map[string]any{
			"attach": map[string]any{
				selectFriendAttachVisited:              newVisited,
				selectFriendAttachClueExchangeExhausted: false,
			},
		},
	}); err != nil {
		log.Error().Err(err).Str("component", selectFriendRecognitionName).Str("name", selected.NameText).Msg("save attach failed")
		return nil, false
	}

	detailJSON, _ := json.Marshal(selected)
	log.Info().
		Str("component", selectFriendRecognitionName).
		Str("name", selected.NameText).
		Ints("button_box", selected.ButtonBox).
		Strs("visited", newVisited).
		Msg("selected friend to click")

	return &maa.CustomRecognitionResult{
		Box:    maa.Rect{selected.ButtonBox[0], selected.ButtonBox[1], selected.ButtonBox[2], selected.ButtonBox[3]},
		Detail: string(detailJSON),
	}, true
}

type selectFriendOCRHit struct {
	Box  []int  `json:"box"`
	Text string `json:"text"`
}

func parseSelectFriendCombinedHits(detail *maa.RecognitionDetail) (buttons, names []selectFriendOCRHit, ok bool) {
	// CombinedResult 与 WithName.all_of 对齐：
	// [0]=进船按钮，[1]=线索交换，[2]=名称 OCR；Results.Best 为空时只能走 DetailJson。
	buttonRaw, ok := selectFriendCombinedDetailJSON(detail, 0, "button")
	if !ok {
		return nil, nil, false
	}
	nameRaw, ok := selectFriendCombinedDetailJSON(detail, 2, "name")
	if !ok {
		return nil, nil, false
	}

	var buttonJSON, nameJSON struct {
		Filtered []selectFriendOCRHit `json:"filtered"`
	}
	if err := json.Unmarshal([]byte(buttonRaw), &buttonJSON); err != nil {
		log.Error().Err(err).Str("component", selectFriendRecognitionName).Msg("parse button detail json")
		return nil, nil, false
	}
	if err := json.Unmarshal([]byte(nameRaw), &nameJSON); err != nil {
		log.Error().Err(err).Str("component", selectFriendRecognitionName).Msg("parse name detail json")
		return nil, nil, false
	}

	// Filtered 缺失时按空切片处理，交给调用方统一判定「无候选」。
	if buttonJSON.Filtered == nil {
		buttonJSON.Filtered = []selectFriendOCRHit{}
	}
	if nameJSON.Filtered == nil {
		nameJSON.Filtered = []selectFriendOCRHit{}
	}
	return buttonJSON.Filtered, nameJSON.Filtered, true
}

func selectFriendCombinedDetailJSON(detail *maa.RecognitionDetail, index int, kind string) (string, bool) {
	if detail == nil {
		log.Warn().Str("component", selectFriendRecognitionName).Str("kind", kind).Msg("combined detail is nil")
		return "", false
	}
	if index < 0 || index >= len(detail.CombinedResult) {
		log.Warn().
			Str("component", selectFriendRecognitionName).
			Str("kind", kind).
			Int("index", index).
			Int("combined_len", len(detail.CombinedResult)).
			Msg("combined result index out of range")
		return "", false
	}
	child := detail.CombinedResult[index]
	if child == nil {
		log.Warn().
			Str("component", selectFriendRecognitionName).
			Str("kind", kind).
			Int("index", index).
			Msg("combined result entry is nil")
		return "", false
	}
	raw := strings.TrimSpace(child.DetailJson)
	if raw == "" {
		log.Warn().
			Str("component", selectFriendRecognitionName).
			Str("kind", kind).
			Int("index", index).
			Msg("combined result DetailJson is empty")
		return "", false
	}
	return raw, true
}

func friendNameHasRemark(name string) bool {
	return strings.Contains(name, "(") || strings.Contains(name, "（")
}

func hitBoxCenterY(box []int) float64 {
	if len(box) < 4 {
		return math.NaN()
	}
	return float64(box[1]) + float64(box[3])/2
}

// nearestButtonBoxByVertical 按纵向中心距离，为名字框找最近的进船按钮。
func nearestButtonBoxByVertical(buttons []selectFriendOCRHit, nameBox []int) ([]int, bool) {
	nameY := hitBoxCenterY(nameBox)
	if math.IsNaN(nameY) || len(buttons) == 0 {
		return nil, false
	}

	bestIdx := -1
	bestDist := math.MaxFloat64
	for i := range buttons {
		if len(buttons[i].Box) < 4 {
			continue
		}
		dist := math.Abs(hitBoxCenterY(buttons[i].Box) - nameY)
		if dist < bestDist {
			bestDist = dist
			bestIdx = i
		}
	}
	if bestIdx < 0 {
		return nil, false
	}
	// 同行一般在几十像素内；过大说明名字与按钮不在同一行，放弃配对。
	const maxRowDist = 80.0
	if bestDist > maxRowDist {
		return nil, false
	}
	return append([]int(nil), buttons[bestIdx].Box...), true
}

func loadSelectFriendVisited(ctx *maa.Context, nodeName string) ([]string, error) {
	raw, err := ctx.GetNodeJSON(nodeName)
	if err != nil {
		return nil, err
	}
	var wrapper struct {
		Attach struct {
			Visited []string `json:"visited"`
		} `json:"attach"`
	}
	if err := json.Unmarshal([]byte(raw), &wrapper); err != nil {
		return nil, err
	}

	out := make([]string, 0, len(wrapper.Attach.Visited))
	seen := make(map[string]struct{}, len(wrapper.Attach.Visited))
	for _, name := range wrapper.Attach.Visited {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		if _, dup := seen[trimmed]; dup {
			continue
		}
		seen[trimmed] = struct{}{}
		out = append(out, trimmed)
	}
	return out, nil
}

// loadClueExchangeExhausted 读取 attach.clue_exchange_exhausted 标志，
// 该标志由 ScrollFinish 的 PipelineOverrideAction 在滚动到底时设为 true。
func loadClueExchangeExhausted(ctx *maa.Context, nodeName string) bool {
	raw, err := ctx.GetNodeJSON(nodeName)
	if err != nil {
		return false
	}
	var wrapper struct {
		Attach struct {
			ClueExchangeExhausted bool `json:"clue_exchange_exhausted"`
		} `json:"attach"`
	}
	if err := json.Unmarshal([]byte(raw), &wrapper); err != nil {
		return false
	}
	return wrapper.Attach.ClueExchangeExhausted
}

func selectFriendVisitedContains(visited []string, name string) bool {
	for _, v := range visited {
		if v == name {
			return true
		}
	}
	return false
}

func buildSelectFriendExpected(visited []string) string {
	escaped := make([]string, 0, len(visited))
	for _, name := range visited {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		escaped = append(escaped, regexp.QuoteMeta(trimmed))
	}
	if len(escaped) == 0 {
		return ".*#.*"
	}
	// 与 DailyEventGoTo 相同：用负向预测排除已访问；Go 侧仍会再按 normalize 过滤一层。
	return fmt.Sprintf("^(?!(?:%s)$).*#.*", strings.Join(escaped, "|"))
}
