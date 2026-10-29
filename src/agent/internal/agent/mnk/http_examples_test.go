package mnk_test

import (
	"aico-agent/internal/agent/mnk"
	"aico-agent/internal/agent/screen"
	"context"
	"log"
	"net/http"
	"time"
)

// ExampleHTTPProvider 展示如何使用 HTTP Provider
func ExampleHTTPProvider() {
	// 客户端：创建 HTTP Provider
	factory := mnk.NewProviderFactory(&screen.ScreenState{})

	provider, err := factory.CreateHTTPProvider(
		"http://localhost:8080", // Bridge server URL
		"example-task-123",      // Task ID for tracking
	)
	if err != nil {
		log.Fatal(err)
	}

	// 使用 HTTP Provider（与本地 Provider 完全相同）
	provider.Click(context.Background(), 500, 500, "left", 0)
	provider.DragStart(context.Background(), 100, 500, "left")
	provider.DragRelease(context.Background(), 900, 500)
	provider.Keypress(context.Background(), []string{"ctrl", "a"})
}

// ExampleHTTPHandler 展示如何创建 HTTP 服务器
func ExampleHTTPHandler() {
	// 创建本地 Provider（HID 或 ADB）
	screenState := &screen.ScreenState{}
	factory := mnk.NewProviderFactory(screenState)

	localProvider, err := factory.CreateHIDProvider(
		"/dev/hidg0",
		"/dev/hidg1",
		"/dev/hidg2",
		true,
		"qwerty",
	)
	if err != nil {
		log.Fatal(err)
	}

	// 创建 HTTP Handler
	handler := mnk.NewHTTPHandler(localProvider)

	// 注册到 HTTP Server
	http.Handle("/api/providers/mnk", handler)

	// 启动服务器
	log.Println("MNK Bridge Server listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// ExampleRegisterHandler 展示如何使用便捷的注册函数
func ExampleRegisterHandler() {
	screenState := &screen.ScreenState{}
	factory := mnk.NewProviderFactory(screenState)

	localProvider, _ := factory.CreateHIDProvider(
		"/dev/hidg0", "/dev/hidg1", "/dev/hidg2",
		true, "qwerty",
	)

	// 创建 ServeMux
	mux := http.NewServeMux()

	// 注册其他路由
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	})

	// 注册 MNK Handler
	mnk.RegisterHandler(mux, localProvider)

	// 启动服务器
	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}
	log.Fatal(server.ListenAndServe())
}

// ExampleBridgeServer 展示完整的 Bridge Server 实现
func Example_bridgeServer() {
	// 1. 初始化 screen state
	screenState := &screen.ScreenState{}
	screenState.Update(1920, 1080)

	// 2. 创建本地 Provider
	factory := mnk.NewProviderFactory(screenState)
	localProvider, err := factory.CreateHIDProvider(
		"/dev/hidg0",
		"/dev/hidg1",
		"/dev/hidg2",
		true,
		"qwerty",
	)
	if err != nil {
		log.Fatalf("Failed to create HID provider: %v", err)
	}

	// 3. 创建 HTTP Server
	mux := http.NewServeMux()

	// Health check endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})

	// MNK Provider endpoint
	mnk.RegisterHandler(mux, localProvider)

	// 4. 配置服务器
	server := &http.Server{
		Addr:           ":8080",
		Handler:        mux,
		ReadTimeout:    30 * time.Second,
		WriteTimeout:   30 * time.Second,
		MaxHeaderBytes: 1 << 20, // 1 MB
	}

	// 5. 启动服务器
	log.Println("Bridge Server starting...")
	log.Println("  - Health check: http://localhost:8080/health")
	log.Println("  - MNK Provider: http://localhost:8080/api/providers/mnk")
	log.Fatal(server.ListenAndServe())
}

// ExampleRemoteClient 展示远程客户端使用
func Example_remoteClient() {
	// 创建 HTTP Provider 连接到远程 Bridge Server
	factory := mnk.NewProviderFactory(&screen.ScreenState{})

	provider, err := factory.CreateHTTPProvider(
		"http://192.168.1.100:8080", // Remote bridge server
		"remote-test-001",           // Task ID
	)
	if err != nil {
		log.Fatal(err)
	}

	// 执行远程操作
	log.Println("Clicking...")
	if err := provider.Click(context.Background(), 500, 500, "left", 0); err != nil {
		log.Printf("Click failed: %v", err)
	}

	log.Println("Dragging...")
	if err := provider.DragStart(context.Background(), 100, 500, "left"); err != nil {
		log.Printf("Drag start failed: %v", err)
	} else if err := provider.DragRelease(context.Background(), 900, 500); err != nil {
		log.Printf("Drag release failed: %v", err)
	}

	log.Println("Typing...")
	if err := provider.Keypress(context.Background(), []string{"ctrl", "a"}); err != nil {
		log.Printf("Keypress failed: %v", err)
	}

	log.Println("Done!")
}

// ExampleDistributedTest 展示分布式测试
func Example_distributedTest() {
	devices := []struct {
		name string
		url  string
	}{
		{"Device 1", "http://device1:8080"},
		{"Device 2", "http://device2:8080"},
		{"Device 3", "http://device3:8080"},
	}

	factory := mnk.NewProviderFactory(&screen.ScreenState{})

	// 并发测试所有设备
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	for _, device := range devices {
		go func(name, url string) {
			log.Printf("Testing %s...", name)

			provider, err := factory.CreateHTTPProvider(url, name)
			if err != nil {
				log.Printf("%s: Failed to connect: %v", name, err)
				return
			}

			// 执行测试操作
			if err := provider.Click(context.Background(), 500, 500, "left", 0); err != nil {
				log.Printf("%s: Click failed: %v", name, err)
				return
			}

			if err := provider.DragStart(context.Background(), 100, 500, "left"); err != nil {
				log.Printf("%s: Drag start failed: %v", name, err)
				return
			}
			if err := provider.DragRelease(context.Background(), 900, 500); err != nil {
				log.Printf("%s: Drag release failed: %v", name, err)
				return
			}

			log.Printf("%s: Test completed successfully", name)
		}(device.name, device.url)
	}

	// 等待所有测试完成或超时
	<-ctx.Done()
}

// ExampleWithAuthentication 展示如何添加认证
func Example_withAuthentication() {
	screenState := &screen.ScreenState{}
	factory := mnk.NewProviderFactory(screenState)
	localProvider, _ := factory.CreateHIDProvider(
		"/dev/hidg0", "/dev/hidg1", "/dev/hidg2",
		true, "qwerty",
	)

	// 创建认证中间件
	authMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// 验证 Authorization header
			token := r.Header.Get("Authorization")
			if token != "Bearer secret-token" {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			next.ServeHTTP(w, r)
		})
	}

	// 应用认证中间件
	mnkHandler := mnk.NewHTTPHandler(localProvider)
	http.Handle("/api/providers/mnk", authMiddleware(mnkHandler))

	log.Fatal(http.ListenAndServe(":8080", nil))
}

// ExampleWithLogging 展示如何添加日志
func Example_withLogging() {
	screenState := &screen.ScreenState{}
	factory := mnk.NewProviderFactory(screenState)
	localProvider, _ := factory.CreateHIDProvider(
		"/dev/hidg0", "/dev/hidg1", "/dev/hidg2",
		true, "qwerty",
	)

	// 创建日志中间件
	loggingMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()

			// 调用下一个处理器
			next.ServeHTTP(w, r)

			// 记录请求
			duration := time.Since(start)
			log.Printf(
				"[MNK] %s %s - %v - Task: %s",
				r.Method,
				r.URL.Path,
				duration,
				r.Header.Get(mnk.BenchmarkTaskIDHeader),
			)
		})
	}

	// 应用日志中间件
	mnkHandler := mnk.NewHTTPHandler(localProvider)
	http.Handle("/api/providers/mnk", loggingMiddleware(mnkHandler))

	log.Fatal(http.ListenAndServe(":8080", nil))
}

// ExampleHTTPProviderConfig 展示配置选项
func Example_httpProviderConfig() {
	factory := mnk.NewProviderFactory(&screen.ScreenState{})

	// 基本配置
	basicProvider, _ := factory.CreateHTTPProvider(
		"http://localhost:8080",
		"",
	)
	_ = basicProvider

	// 高级配置
	advancedProvider := mnk.NewHTTPProvider(mnk.HTTPProviderConfig{
		BaseURL: "http://remote-device:8080",
		Timeout: 60 * time.Second, // 长超时（用于慢速网络）
		TaskID:  "benchmark-run-001",
	})
	_ = advancedProvider

	// 使用 ProviderConfig
	config := mnk.ProviderConfig{
		Backend:     "http",
		HTTPBaseURL: "http://localhost:8080",
		HTTPTaskID:  "task-123",
	}
	configProvider, _ := factory.CreateProvider(config)
	_ = configProvider
}
