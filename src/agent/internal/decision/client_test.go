package decision

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func jpegBase64(t *testing.T, width, height int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func TestAskSendsTypedQuestionsWithImagesHeader(t *testing.T) {
	var got Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/systemone" || r.Method != http.MethodPost {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer ts-key" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get(imagesHeader) != "true" {
			t.Errorf("images header missing")
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = io.WriteString(w, `{"model":"jev-1.14.0","answers":{"open":{"type":"noul","noul":0.93},`+
			`"where":{"type":"box","box":{"top":0.1,"bottom":0.3,"left":0.2,"right":0.6},"confidence":0.8}},`+
			`"usage":{"input_tokens":10,"output_tokens":2}}`)
	}))
	defer server.Close()

	shot, err := PrepareJPEG(jpegBase64(t, 400, 600))
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{BaseURL: server.URL + "/v1/", APIKey: "ts-key", Model: "jev-preview", HTTP: server.Client()}
	resp, err := client.Ask(context.Background(), Request{
		State:  map[string]any{"screen": shot.Ref},
		Assets: map[string]Asset{shot.Ref: shot.Asset},
		Questions: map[string]Question{
			"open":  {Type: TypeNoul, Instructions: "Is the app open in `screen`?"},
			"where": {Type: TypeBox, Instructions: "the search field in `screen`"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "jev-preview" || got.Assets[shot.Ref].Format != "jpg" || got.Questions["where"].Type != TypeBox {
		t.Fatalf("request = %+v", got)
	}
	open, err := resp.Answer("open", TypeNoul)
	if err != nil || *open.Noul != 0.93 {
		t.Fatalf("open = %+v, %v", open, err)
	}
	where, err := resp.Answer("where", TypeBox)
	if err != nil {
		t.Fatal(err)
	}
	if x, y := where.Box.Center1000(); x != 400 || y != 200 {
		t.Fatalf("center = %v,%v", x, y)
	}
	if _, err := resp.Answer("open", TypeChoice); err == nil {
		t.Fatal("expected a type mismatch error")
	}
}

func TestAskRetriesOverloadAndReportsOtherErrors(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(imagesHeader) != "" {
			t.Errorf("text-only request sent the images header")
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(529)
			return
		}
		_, _ = io.WriteString(w, `{"answers":{"q":{"type":"choice","choice":"a","confidence":0.7,"probabilities":{"a":0.8,"b":0.2}}}}`)
	}))
	defer server.Close()
	client := &Client{BaseURL: server.URL, APIKey: "k", Model: "m", HTTP: server.Client()}
	resp, err := client.Ask(context.Background(), Request{State: "s", Questions: map[string]Question{
		"q": {Type: TypeChoice, Instructions: "pick", Criteria: map[string]any{"a": nil, "b": nil}},
	}})
	if err != nil || calls.Load() != 2 {
		t.Fatalf("err = %v, calls = %d", err, calls.Load())
	}
	if answer, _ := resp.Answer("q", TypeChoice); !answer.Confident(0.7) || answer.Confident(0.71) {
		t.Fatalf("confidence = %+v", answer)
	}

	denied := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnavailableForLegalReasons)
		_, _ = io.WriteString(w, `{"title":"Typesafe is not available in your region."}`)
	}))
	defer denied.Close()
	client = &Client{BaseURL: denied.URL, APIKey: "k", Model: "m", HTTP: denied.Client()}
	_, err = client.Ask(context.Background(), Request{State: "s", Questions: map[string]Question{"q": {Type: TypeNoul, Instructions: "?"}}})
	if err == nil || !strings.Contains(err.Error(), "451") || !strings.Contains(err.Error(), "region") {
		t.Fatalf("err = %v", err)
	}
}

func TestPrepareJPEGFitsLimits(t *testing.T) {
	tall, err := PrepareJPEG(jpegBase64(t, 1080, 2400))
	if err != nil {
		t.Fatal(err)
	}
	if tall.Height != maxImageSide || tall.Width < minImageSide || tall.Width*tall.Height > maxImagePixels {
		t.Fatalf("tall = %dx%d", tall.Width, tall.Height)
	}
	decoded, _ := base64.StdEncoding.DecodeString(tall.Asset.DataBase64)
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(decoded))
	if err != nil || cfg.Width != tall.Width || cfg.Height != tall.Height {
		t.Fatalf("encoded = %+v, %v", cfg, err)
	}

	original := jpegBase64(t, 480, 1000)
	kept, err := PrepareJPEG(original)
	if err != nil || kept.Width != 480 || kept.Height != 1000 || kept.Asset.DataBase64 != original {
		t.Fatalf("kept = %dx%d same=%v, %v; an image within the limits must be sent unchanged", kept.Width, kept.Height, kept.Asset.DataBase64 == original, err)
	}
	if _, err := PrepareJPEG(jpegBase64(t, 200, 600)); err == nil {
		t.Fatal("expected a too-narrow image to be rejected")
	}
}

func TestNoulConfident(t *testing.T) {
	for _, tc := range []struct {
		p         float64
		yes, sure bool
	}{{0.9, true, true}, {0.1, false, true}, {0.6, true, false}, {0.4, false, false}} {
		yes, sure := NoulConfident(tc.p, 0.8)
		if yes != tc.yes || sure != tc.sure {
			t.Errorf("NoulConfident(%v) = %v,%v", tc.p, yes, sure)
		}
	}
}

func TestAskOpenAITranslatesQuestionsAndAnswers(t *testing.T) {
	var got openAIRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/decisions" || r.Header.Get("Authorization") != "Bearer sk-test" || r.Header.Get(imagesHeader) != "" {
			t.Errorf("request = %s %s headers=%v", r.Method, r.URL.Path, r.Header)
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("decode: %v", err)
		}
		_, _ = io.WriteString(w, `{"model":"gpt-6-luna","answers":[`+
			`{"type":"predicate","name":"found","probability":0.91},`+
			`{"type":"choice","name":"mode","choice":"ascii","confidence":0.88,"probabilities":[{"value":"ascii","probability":0.94},{"value":"composition","probability":0.06}]},`+
			`{"type":"refusal","name":"other"}],"usage":{"input_tokens":12}}`)
	}))
	defer server.Close()

	shot, err := PrepareJPEG(jpegBase64(t, 400, 600))
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{Provider: ProviderOpenAI, BaseURL: server.URL + "/v1", APIKey: "sk-test", Model: "gpt-6-luna", HTTP: server.Client()}
	resp, err := client.Ask(context.Background(), Request{
		State:  map[string]any{"screen": shot.Ref, "query": "微信"},
		Assets: map[string]Asset{shot.Ref: shot.Asset},
		Questions: map[string]Question{
			"found": {Type: TypeNoul, Instructions: "Is `query` visible in `screen`?"},
			"where": {Type: TypeBox, Instructions: "the result in `screen`"},
			"mode":  {Type: TypeChoice, Instructions: "Which mode?", Criteria: map[string]any{"ascii": "plain", "composition": nil}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "gpt-6-luna" || len(got.Questions) != 2 {
		t.Fatalf("questions = %+v; the box question must be left out", got.Questions)
	}
	if got.Questions[0].Name != "found" || got.Questions[0].Type != "predicate" || got.Questions[1].Type != "choice" || len(got.Questions[1].Choices) != 2 {
		t.Fatalf("questions = %+v", got.Questions)
	}
	parts := got.Input[0].Content
	if len(parts) != 3 || !strings.Contains(parts[0].Text, `"query":"微信"`) || parts[1].Text != "`screen` is this image:" ||
		!strings.HasPrefix(parts[2].ImageURL, "data:image/jpeg;base64,") {
		t.Fatalf("input = %+v", parts)
	}
	found, err := resp.Answer("found", TypeNoul)
	if err != nil || *found.Noul != 0.91 {
		t.Fatalf("found = %+v, %v", found, err)
	}
	mode, err := resp.Answer("mode", TypeChoice)
	if err != nil || mode.Choice != "ascii" || !mode.Confident(0.8) || mode.Probabilities["composition"] != 0.06 {
		t.Fatalf("mode = %+v, %v", mode, err)
	}
	if _, err := resp.Answer("where", TypeBox); err == nil {
		t.Fatal("OpenAI must not produce a box answer")
	}
}
