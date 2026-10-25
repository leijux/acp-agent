package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/leijux/acp-agent/internal/mcp"
	"github.com/leijux/acp-agent/internal/tools"
)

// connectMCP 按配置连接所有 MCP server，并把它们的工具注册进 reg。
//
// 设计要点：
//   - 连接失败不阻断启动。远程 server 常因网络、凭据、依赖缺失不可用，
//     为此让整个 CLI 起不来是过度反应；错误打到 stderr 即可。
//   - 注册发生在 wrapRegistry 之前（调用方保证），所以远程工具和内置
//     工具共享同一套权限门禁。
//   - 返回的 Manager 由 app 持有，退出时 Close 回收子进程树。
func connectMCP(ctx context.Context, cfg Config, reg *tools.Registry, term *terminal) (*mcp.Manager, error) {
	if cfg.NoMCP {
		return nil, nil
	}
	mcpCfg, err := loadMCPConfig(cfg)
	if err != nil {
		return nil, fmt.Errorf("加载 MCP 配置: %w", err)
	}
	if len(mcpCfg.MCPServers) == 0 {
		return nil, nil
	}
	if err := mcpCfg.Validate(); err != nil {
		return nil, fmt.Errorf("MCP 配置有误: %w", err)
	}

	mgr := mcp.NewManager(mcpCfg,
		mcp.WithConnectTimeout(30*time.Second),
		mcp.WithServerErrorHandler(func(name string, err error) {
			term.logf("MCP %s: %v\n", name, err)
		}),
	)
	// Connect 返回聚合错误（部分 server 失败），这些不该挡住启动。
	if err := mgr.Connect(ctx); err != nil {
		term.logf("MCP: %v\n", err)
	}

	if skipped, err := mgr.RegisterInto(reg); err != nil {
		term.logf("MCP: 注册工具失败: %v\n", err)
	} else if len(skipped) > 0 {
		term.logf("MCP: 以下工具与内置工具重名，已跳过: %v\n", skipped)
	}

	// 远端 tools/list_changed 时同步 Registry。
	//
	// 注意 reg 是「底层」注册表，上层还有一层权限门禁视图（wrapRegistry 用
	// Registry.UseBase 建的是视图而不是拷贝），所以这里的增删会自动穿透到
	// 模型看到的工具表 —— 不需要也不应该再去通知视图。
	mgr.SetToolsChangedHandler(func([]tools.Tool) {
		if err := mgr.SyncRegistry(reg); err != nil {
			term.logf("MCP: 同步工具失败: %v\n", err)
		}
	})

	if len(mgr.Tools()) > 0 {
		connected := 0
		for _, st := range mgr.Status() {
			if st.Connected {
				connected++
			}
		}
		term.logf("MCP: %d 个 server，%d 个工具（%s）\n",
			connected, len(mgr.Tools()), describeSources(mcpCfg))
	}
	return mgr, nil
}

// describeSources 把加载的配置文件拼成一行，没有时返回 "无配置文件"。
func describeSources(cfg *mcp.Config) string {
	if len(cfg.SourceFiles) == 0 {
		return "无配置文件"
	}
	return strings.Join(cfg.SourceFiles, ", ")
}

// closeMCP 结束所有 MCP 会话（含 stdio 子进程树）。
func closeMCP(mgr *mcp.Manager) {
	if mgr == nil {
		return
	}
	_ = mgr.Close()
}

// runMCPStatus 连接所有配置的 MCP server，打印状态与工具清单后退出。
// 排查「配置写对了但模型看不到工具」这类问题的最短路径。
func runMCPStatus(ctx context.Context, cfg Config) error {
	if cfg.NoMCP {
		fmt.Println("--no-mcp：未连接任何 MCP server")
		return nil
	}
	mcpCfg, err := loadMCPConfig(cfg)
	if err != nil {
		return err
	}
	fmt.Println("MCP 配置来源:", describeSources(mcpCfg))
	if cfg.MCPConfig != "" {
		fmt.Println("（由 --mcp-config 指定）")
	} else {
		fmt.Printf("搜索顺序: %s\n", strings.Join(mcp.DefaultPaths(cfg.Root), "  →  "))
	}
	if len(mcpCfg.MCPServers) == 0 {
		fmt.Println("未配置任何 MCP server。")
		return nil
	}
	if err := mcpCfg.Validate(); err != nil {
		fmt.Printf("配置校验警告: %v\n", err)
	}

	mgr := mcp.NewManager(mcpCfg, mcp.WithConnectTimeout(30*time.Second))
	if err := mgr.Connect(ctx); err != nil {
		fmt.Printf("连接警告: %v\n", err)
	}
	defer mgr.Close()

	for _, st := range mgr.Status() {
		state := "✗ 未连接"
		if st.Connected {
			state = "✓ 已连接"
		}
		fmt.Printf("\n[%s] %s  (%s)\n", st.Name, state, st.Transport)
		if st.ServerName != "" {
			fmt.Printf("  server: %s %s  protocol=%s\n", st.ServerName, st.Version, st.Protocol)
		}
		if st.Err != nil {
			fmt.Printf("  错误: %v\n", st.Err)
		}
		for _, tool := range mgr.Tools() {
			if !strings.HasPrefix(tool.Name(), "mcp__"+st.Name+"__") {
				continue
			}
			flag := ""
			if m, ok := tool.(tools.Mutation); ok && m.Mutates() {
				flag = " [需确认]"
			}
			desc := tool.Description()
			if len(desc) > 70 {
				desc = desc[:70] + "…"
			}
			fmt.Printf("  - %s%s\n    %s\n", tool.Name(), flag, desc)
		}
	}
	return nil
}

func loadMCPConfig(cfg Config) (*mcp.Config, error) {
	if cfg.MCPConfig != "" {
		return mcp.Load(cfg.MCPConfig)
	}
	return mcp.LoadFiles(mcp.DefaultPaths(cfg.Root)...)
}
