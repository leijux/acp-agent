package proc

import (
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// liveCmd 起一个持续一小段时间的进程。
func liveCmd(t *testing.T) *exec.Cmd {
	t.Helper()
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("ping", "-n", "5", "127.0.0.1")
	} else {
		cmd = exec.Command("sleep", "3")
	}
	if err := cmd.Start(); err != nil {
		t.Skipf("无法启动测试进程: %v", err)
	}
	t.Cleanup(func() { KillTree(cmd); _ = cmd.Wait() })
	return cmd
}

// TestAliveDetectsRunningProcess 回归：Windows 上曾恒返回 false。
func TestAliveDetectsRunningProcess(t *testing.T) {
	cmd := liveCmd(t)
	if !Alive(cmd.Process) {
		t.Fatalf("%s 上把正在运行的进程判成已退出", runtime.GOOS)
	}
}

// TestAliveDetectsExitedProcess 已退出的进程必须判死。
func TestAliveDetectsExitedProcess(t *testing.T) {
	cmd := liveCmd(t)
	KillTree(cmd)
	_ = cmd.Wait()

	// 进程刚退出时句柄可能仍未回收，给一点时间。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if !Alive(cmd.Process) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("进程已退出，Alive 仍返回 true")
}

// TestAliveNil 空指针不该 panic。
func TestAliveNil(t *testing.T) {
	if Alive(nil) {
		t.Fatal("nil 进程应返回 false")
	}
}

// TestKillTreeStopsGrandchild 杀整棵树：外壳进程拉起的子进程也要没了。
func TestKillTreeStopsGrandchild(t *testing.T) {
	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.Command("cmd", "/c", "ping -n 30 127.0.0.1")
	} else {
		cmd = exec.Command("sh", "-c", "sleep 30 & wait")
	}
	Setup(cmd)
	if err := cmd.Start(); err != nil {
		t.Skipf("无法启动: %v", err)
	}

	KillTree(cmd)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("KillTree 之后进程仍未退出")
	}
}
