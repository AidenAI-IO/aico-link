package mnk_test

import (
	"aico-agent/internal/agent/mnk"
	"aico-agent/internal/agent/screen"
	"context"
	"fmt"
	"log"
)

// ExampleHIDProvider 展示如何创建和使用 HID Provider
func ExampleHIDProvider() {
	// 创建 screen state（用于坐标转换）
	screenState := &screen.ScreenState{}
	screenState.Update(1920, 1080) // 设置屏幕尺寸

	// 创建 Provider Factory
	factory := mnk.NewProviderFactory(screenState)

	// 创建 HID Provider
	provider, err := factory.CreateHIDProvider(
		"/dev/hidg0", // 指针设备
		"/dev/hidg1", // 键盘设备
		"/dev/hidg2", // Android 扩展键盘设备
		true,         // touchscreen 模式
		"qwerty",     // 键盘布局
	)
	if err != nil {
		log.Fatalf("创建 HID Provider 失败: %v", err)
	}

	// 使用示例

	// 1. 点击
	err = provider.Click(context.Background(), 500, 500, "left", 0)
	if err != nil {
		log.Printf("点击失败: %v", err)
	}

	// 2. 长按
	err = provider.Click(context.Background(), 500, 500, "left", 500) // 按住 500ms
	if err != nil {
		log.Printf("长按失败: %v", err)
	}

	// 3. 双击
	err = provider.DoubleClick(context.Background(), 300, 400, "left")
	if err != nil {
		log.Printf("双击失败: %v", err)
	}

	// 4. 跨截图确认目标的拖放
	err = provider.DragStart(context.Background(), 100, 500, "left")
	if err != nil {
		log.Printf("开始拖动失败: %v", err)
	}
	err = provider.DragRelease(context.Background(), 900, 500)
	if err != nil {
		log.Printf("释放拖动失败: %v", err)
	}

	// 5. 按键
	err = provider.Keypress(context.Background(), []string{"enter"})
	if err != nil {
		log.Printf("按键失败: %v", err)
	}

	// 6. 组合键
	err = provider.Keypress(context.Background(), []string{"ctrl", "a"})
	if err != nil {
		log.Printf("组合键失败: %v", err)
	}

	// 7. 光标移动（仅 absolute mode）
	err = provider.Move(context.Background(), 500, 300)
	if err != nil {
		log.Printf("移动失败: %v", err)
	}

	// 8. 滚动
	err = provider.Scroll(context.Background(), 0, -3)
	if err != nil {
		log.Printf("滚动失败: %v", err)
	}
}

// ExampleADBProvider 展示如何创建和使用 ADB Provider
func ExampleADBProvider() {
	// 创建 screen state
	screenState := &screen.ScreenState{}
	screenState.Update(1080, 1920)

	// 创建 Provider Factory
	factory := mnk.NewProviderFactory(screenState)

	// 创建 ADB Provider
	provider, err := factory.CreateADBProvider()
	if err != nil {
		log.Fatalf("创建 ADB Provider 失败: %v", err)
	}

	// 使用方法与 HID Provider 完全相同
	err = provider.Click(context.Background(), 500, 500, "left", 0)
	if err != nil {
		log.Printf("点击失败: %v", err)
	}

	err = provider.DragStart(context.Background(), 500, 800, "left")
	if err == nil {
		err = provider.DragRelease(context.Background(), 500, 200)
	}
	if err != nil {
		log.Printf("拖动失败: %v", err)
	}

	err = provider.Keypress(context.Background(), []string{"android_back"})
	if err != nil {
		log.Printf("返回键失败: %v", err)
	}
}

// Example_toolIntegration 展示如何在工具中集成 MNK Provider
func Example_toolIntegration() {
	// 创建 Provider
	screenState := &screen.ScreenState{}
	screenState.Update(1920, 1080)
	factory := mnk.NewProviderFactory(screenState)
	provider, _ := factory.CreateHIDProvider(
		"/dev/hidg0",
		"/dev/hidg1",
		"/dev/hidg2",
		true,
		"qwerty",
	)

	// 创建工具适配器
	touchGestureTool := mnk.NewTouchGestureToolAdapter(provider)

	keyboardTapTool := mnk.NewKeyboardTapToolAdapter(provider)
	mouseMoveTool := mnk.NewMouseMoveToolAdapter(provider)
	mouseScrollTool := mnk.NewMouseScrollToolAdapter(provider)

	// 使用工具
	ctx := context.Background()

	// Touch gesture - tap
	result, err := touchGestureTool.Call(ctx, `{"type":"tap","point":{"x":500,"y":500}}`)
	fmt.Printf("Tap result: %s, error: %v\n", result, err)

	// Touch gesture - swipe
	result, err = touchGestureTool.Call(ctx, `{"type":"swipe","start":{"x":100,"y":500},"end":{"x":900,"y":500}}`)
	fmt.Printf("Swipe result: %s, error: %v\n", result, err)

	// Keyboard tap
	result, err = keyboardTapTool.Call(ctx, `{"keys":["ctrl","a"]}`)
	fmt.Printf("Keyboard result: %s, error: %v\n", result, err)

	// Mouse move
	result, err = mouseMoveTool.Call(ctx, `{"x":250,"y":750}`)
	fmt.Printf("Move result: %s, error: %v\n", result, err)

	// Mouse scroll
	result, err = mouseScrollTool.Call(ctx, `{"delta":-3}`)
	fmt.Printf("Scroll result: %s, error: %v\n", result, err)
}

// ExampleProviderFactory 展示如何使用配置创建 Provider
func ExampleProviderFactory() {
	screenState := &screen.ScreenState{}
	screenState.Update(1920, 1080)
	factory := mnk.NewProviderFactory(screenState)

	// 配置 HID Provider
	hidConfig := mnk.ProviderConfig{
		Backend:               "hid",
		PointerDevice:         "/dev/hidg0",
		KeyboardDevice:        "/dev/hidg1",
		AndroidKeyboardDevice: "/dev/hidg2",
		Touchscreen:           true,
		KeyboardLayout:        "qwerty",
	}

	hidProvider, err := factory.CreateProvider(hidConfig)
	if err != nil {
		log.Fatalf("创建 HID Provider 失败: %v", err)
	}

	// 配置 ADB Provider
	adbConfig := mnk.ProviderConfig{
		Backend: "adb",
	}

	adbProvider, err := factory.CreateProvider(adbConfig)
	if err != nil {
		log.Fatalf("创建 ADB Provider 失败: %v", err)
	}

	// 使用 Provider
	_ = hidProvider
	_ = adbProvider
}

// ExampleAdvancedGestures 展示需要观察目标位置的拖放手势
func Example_advancedGestures() {
	screenState := &screen.ScreenState{}
	factory := mnk.NewProviderFactory(screenState)
	provider, _ := factory.CreateHIDProvider("/dev/hidg0", "/dev/hidg1", "", true, "qwerty")

	provider.DragStart(context.Background(), 500, 200, "left")
	// 检查 drag_start 后的截图并确认最终目标，再释放。
	provider.DragRelease(context.Background(), 900, 800)
}

// ExampleErrorHandling 展示错误处理
func Example_errorHandling() {
	screenState := &screen.ScreenState{}
	factory := mnk.NewProviderFactory(screenState)
	provider, err := factory.CreateHIDProvider("/dev/hidg0", "/dev/hidg1", "", true, "qwerty")
	if err != nil {
		log.Fatalf("创建 Provider 失败: %v", err)
	}

	// 处理坐标超出范围
	err = provider.Click(context.Background(), 1500, 500, "left", 0) // 超出 0-1000 范围
	if err != nil {
		log.Printf("坐标错误: %v", err)
		// 应该返回类似 "coordinates must be in range 0-1000" 的错误
	}

	// 处理没有活动拖动时的释放
	err = provider.DragRelease(context.Background(), 500, 500)
	if err != nil {
		log.Printf("拖动状态错误: %v", err)
	}

	// 处理无效按键
	err = provider.Keypress(context.Background(), []string{"invalid_key"})
	if err != nil {
		log.Printf("按键错误: %v", err)
		// 应该返回 "unknown key" 的错误
	}

	// 处理 ADB 特定错误
	adbProvider, _ := factory.CreateADBProvider()
	err = adbProvider.Move(context.Background(), 500, 500) // ADB 不支持 Move
	if err != nil {
		log.Printf("不支持的操作: %v", err)
		// 应该返回 "move is unsupported on adb" 的错误
	}
}

// ExamplePerformanceOptimization 展示性能优化技巧
func Example_performanceOptimization() {
	screenState := &screen.ScreenState{}
	factory := mnk.NewProviderFactory(screenState)
	provider, _ := factory.CreateHIDProvider("/dev/hidg0", "/dev/hidg1", "", true, "qwerty")

	// 1. 拖放只使用一次 start/release，并在两者之间确认目标。
	provider.DragStart(context.Background(), 100, 500, "left")
	provider.DragRelease(context.Background(), 900, 500)

	// 2. 复用 Provider 实例
	// 不要在每次调用时都创建新的 Provider

	// 3. 预设屏幕尺寸
	// 在应用启动时设置一次，避免重复查询
	screenState.Update(1920, 1080)
	screenState.UpdateActiveArea(1920, 1080, screen.ScreenActiveArea{
		Valid:  true,
		X:      0,
		Y:      0,
		Width:  1920,
		Height: 1080,
	})
}
