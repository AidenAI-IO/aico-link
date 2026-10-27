package agent

import (
	"context"
	"io"

	"aico-agent/internal/agent/screenprovider"
)

type frameMetadata = screenprovider.FrameMetadata
type screenCaptureInfo = screenprovider.CaptureInfo
type adbDeviceInfo = screenprovider.DeviceInfo
type FrameHealthResult = screenprovider.HealthResult
type ScreenCaptureClient = screenprovider.Fallback
type ADBScreenClient = screenprovider.ADB
type FrameServiceClient = screenprovider.FrameService

func NewScreenCaptureClient(socketPath string) *screenprovider.Fallback {
	return screenprovider.NewFallback(socketPath)
}

func newScreenCaptureClient(primary, fallback screenprovider.Source) *screenprovider.Fallback {
	return screenprovider.NewFallbackFromSources(primary, fallback)
}

func NewADBScreenClient() *screenprovider.ADB {
	return screenprovider.NewADB()
}

func NewFrameServiceClient(socketPath string) *screenprovider.FrameService {
	return screenprovider.NewFrameService(socketPath)
}

func cloneScreenCaptureInfo(info screenCaptureInfo) screenCaptureInfo {
	return screenprovider.CloneCaptureInfo(info)
}

func cloneADBDeviceInfo(info *adbDeviceInfo) *adbDeviceInfo {
	return screenprovider.CloneDeviceInfo(info)
}

func runADBCommand(ctx context.Context, adbPath string, args ...string) ([]byte, []byte, error) {
	return screenprovider.RunADB(ctx, adbPath, args...)
}

func clearAutoConfiguredADBSerial(serial string) {
	screenprovider.ClearAutoConfiguredSerial(serial)
}

func readUDSMessage(r io.Reader) ([]byte, []byte, error) {
	return screenprovider.ReadUDSMessage(r)
}

func writeUDSMessage(w io.Writer, header, payload []byte) error {
	return screenprovider.WriteUDSMessage(w, header, payload)
}
