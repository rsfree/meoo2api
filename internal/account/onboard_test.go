package account

import (
	"sync/atomic"
	"testing"
	"time"
)

// 接入成功后必须自动领当日积分，并把本账号自己的邀请码落库（好当下一批的种子）。
func TestOnboardAfterAttachCheckinAndInviteCode(t *testing.T) {
	svc, initHits := newTaskSvc(t, true /*loggedIn*/, true /*granted*/, true /*grantToday*/)

	svc.onboardAfterAttach("a_1")

	// 异步执行：轮询等两件事都落盘，最多 3 秒
	deadline := time.Now().Add(3 * time.Second)
	var gotDate, gotCode string
	for time.Now().Before(deadline) {
		svc.St.Mu.RLock()
		for _, a := range svc.St.Data.Accounts {
			if a.ID == "a_1" {
				gotDate, gotCode = a.CheckinDate, a.InviteCode
				break
			}
		}
		svc.St.Mu.RUnlock()
		if gotDate != "" && gotCode != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if n := atomic.LoadInt32(initHits); n == 0 {
		t.Fatal("应调用 entitlement/init 领积分，实际一次没打")
	}
	if gotDate == "" {
		t.Fatal("签到结果没落盘（CheckinDate 仍为空）")
	}
	if gotCode != "AB12CD34" {
		t.Fatalf("邀请码应落库为 AB12CD34，实际 %q", gotCode)
	}
}

// 设备授权只有 AK、没有 Cookie。entitlement/init 对 AK 一律 403，所以不该白打一次，
// 但要在 checkin_err 上留下可读原因，别让面板显示成「没签过」。
func TestOnboardAfterAttachWithoutCookieSkipsUpstream(t *testing.T) {
	svc, initHits := newTaskSvc(t, true, true, true)
	svc.St.Mu.Lock()
	svc.St.Data.Accounts[0].Cookie = ""
	svc.St.Mu.Unlock()

	svc.onboardAfterAttach("a_1")

	deadline := time.Now().Add(2 * time.Second)
	var gotErr string
	for time.Now().Before(deadline) {
		svc.St.Mu.RLock()
		for _, a := range svc.St.Data.Accounts {
			if a.ID == "a_1" {
				gotErr = a.CheckinErr
			}
		}
		svc.St.Mu.RUnlock()
		if gotErr != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	if n := atomic.LoadInt32(initHits); n != 0 {
		t.Fatalf("无 Cookie 不该请求签到接口，实际打了 %d 次", n)
	}
	if gotErr == "" {
		t.Fatal("无 Cookie 时应落一条可读的 checkin_err")
	}
}

// 空账号 ID 直接返回，不能起 goroutine 空转。
func TestOnboardAfterAttachEmptyIDIsNoop(t *testing.T) {
	svc, initHits := newTaskSvc(t, true, true, true)
	svc.onboardAfterAttach("")
	time.Sleep(150 * time.Millisecond)
	if n := atomic.LoadInt32(initHits); n != 0 {
		t.Fatalf("空账号 ID 不该触发任何请求，实际 %d 次", n)
	}
}
