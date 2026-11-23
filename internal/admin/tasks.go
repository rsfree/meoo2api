package admin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"meoo2api/internal/model"
)

// 任务中心：签到（领每日登录积分）与邀请。两者都只能凭网页 Cookie 调用上游，
// 所以这里除了转发，还负责把"Cookie 过期"这种用户可自修的状态原样传回面板。

func (h *Handler) tasksOverview(w http.ResponseWriter, r *http.Request) {
	// 每个账号两次只读调用；账号多时也要给足时间，但不至于把浏览器拖死。
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	rows := h.Acc.TaskOverview(ctx)
	writeJSON(w, 200, map[string]any{"ok": true, "now": model.NowMilli(), "accounts": rows})
}

func (h *Handler) tasksCheckin(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := decodeJSON(r, &in); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, 400, "bad_request", "请求格式错误")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 300*time.Second)
	defer cancel()

	if in.ID != "" {
		res := h.Acc.Checkin(ctx, in.ID)
		signed, amount := 0, 0.0
		if res.OK {
			signed, amount = 1, res.Amount
		}
		writeJSON(w, 200, map[string]any{
			"ok": true, "results": []any{res}, "signed": signed, "amount": amount,
		})
		return
	}

	results := h.Acc.CheckinAll(ctx)
	signed, amount := 0, 0.0
	for _, r := range results {
		if r.OK {
			signed++
			amount += r.Amount
		}
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "results": results, "signed": signed, "amount": amount,
	})
}

func (h *Handler) tasksInvite(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID     string `json:"id"`
		Action string `json:"action"` // generate | card（默认）
	}
	if err := decodeJSON(r, &in); err != nil {
		writeErr(w, 400, "bad_request", "请求格式错误")
		return
	}
	if in.ID == "" {
		writeErr(w, 400, "bad_request", "缺少账号")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	var (
		out map[string]any
		err error
	)
	if in.Action == "generate" {
		out, err = h.Acc.InviteGenerate(ctx, in.ID)
	} else {
		out, err = h.Acc.InviteCard(ctx, in.ID)
	}
	if err != nil {
		// 200 + ok:false：面板统一按业务失败提示，不把它当网络错误。
		// card 可能只有 reward_config（掉线时用来显示奖励规则），照原样带回去。
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error(), "card": out})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "card": out})
}
