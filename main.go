// Command genimage is GenImage MCP: a macOS desktop app (viva) that exposes
// OpenAI-compatible image generation as MCP tools over Streamable HTTP.
//
//	genimage                 open the desktop window
//	genimage serve           run the MCP server without a window (dev / launchd)
//	genimage version         print the version
//
// Flags (lowest priority; the config file and environment variables win):
//
//	--config PATH  --host HOST  --port N  --output-dir DIR  --default-model ID  --autostart
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/972579/open-image-mcp/internal/appcore"
	"github.com/972579/open-image-mcp/internal/config"
	"github.com/972579/open-image-mcp/internal/gui"
	"github.com/972579/open-image-mcp/internal/mcpserver"
)

// main dispatches to the GUI, the headless server or the version command.
func main() {
	args := os.Args[1:]
	cmd := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	// macOS passes -psn_* when an app is opened from Finder on old systems.
	args = dropPSN(args)

	overrides, err := parseFlags(cmd, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	opts := config.LoadOptions{Flags: overrides}

	switch cmd {
	case "version":
		fmt.Println("gen-openai-image", mcpserver.Version)
	case "serve":
		if err := serve(opts); err != nil {
			fmt.Fprintln(os.Stderr, "错误:", err)
			os.Exit(1)
		}
	case "":
		core, err := appcore.Open(opts, false)
		if err != nil {
			fmt.Fprintln(os.Stderr, "错误:", err)
			os.Exit(1)
		}
		defer core.Close()
		if err := gui.Run(core); err != nil {
			fmt.Fprintln(os.Stderr, "错误:", err)
			os.Exit(1)
		}
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q；可用：serve、version\n", cmd)
		os.Exit(2)
	}
}

// parseFlags reads the startup flags. Only flags that were actually given are
// set in the returned overrides, so unset flags never mask the file.
//
// cmd is the subcommand (for the usage line); args are the remaining
// arguments. It returns flag.ErrHelp for -h.
func parseFlags(cmd string, args []string) (config.Overrides, error) {
	fs := flag.NewFlagSet("genimage "+cmd, flag.ContinueOnError)
	path := fs.String("config", "", "配置文件路径（默认 ~/.gen-openai-image/config.yaml）")
	host := fs.String("host", "", "MCP 服务监听地址")
	port := fs.Int("port", 0, "MCP 服务端口")
	outDir := fs.String("output-dir", "", "图片保存目录")
	model := fs.String("default-model", "", "默认模型")
	autostart := fs.Bool("autostart", false, "打开 App 时自动启动 MCP 服务")
	if err := fs.Parse(args); err != nil {
		return config.Overrides{}, err
	}
	o := config.Overrides{ConfigPath: *path}
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "host":
			o.Host = host
		case "port":
			o.Port = port
		case "output-dir":
			o.OutputDir = outDir
		case "default-model":
			o.DefaultModel = model
		case "autostart":
			o.Autostart = autostart
		}
	})
	return o, nil
}

// serve runs the MCP server without a window until SIGINT/SIGTERM, logging to
// stderr as well as the log file.
func serve(opts config.LoadOptions) error {
	core, err := appcore.Open(opts, true)
	if err != nil {
		return err
	}
	defer core.Close()
	if err := core.Loaded().Effective.Validate(); err != nil {
		return err
	}
	if err := core.StartServer(); err != nil {
		return err
	}
	st := core.Manager.Status()
	fmt.Fprintf(os.Stderr, "GenImage MCP %s 已启动: %s\n%s\n按 Ctrl+C 停止。\n", mcpserver.Version, st.URL, core.Describe())
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()
	fmt.Fprintln(os.Stderr, "正在停止…")
	return nil
}

// dropPSN removes the -psn_X_Y argument macOS may add for Finder launches.
func dropPSN(args []string) []string {
	out := args[:0]
	for _, a := range args {
		if !strings.HasPrefix(a, "-psn_") {
			out = append(out, a)
		}
	}
	return out
}
