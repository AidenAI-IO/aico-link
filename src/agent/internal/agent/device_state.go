package agent

import "time"

type deviceStateUpdater struct {
	config   Config
	snapshot func() Config
}

func newDeviceStateUpdater(config Config) *deviceStateUpdater {
	return &deviceStateUpdater{config: config}
}

func (u *deviceStateUpdater) UpdateState() map[string]string {
	if u == nil {
		return nil
	}
	cfg := u.config
	if u.snapshot != nil {
		cfg = u.snapshot()
	}
	return map[string]string{
		"device_type":         cfg.DeviceTypeOrDefault(),
		"device_platform":     cfg.DevicePlatformOrDefault(),
		"device_pointer_mode": cfg.PointerModeOrDefault(),
		"controller_time":     controllerTimeState(promptNow(), cfg.TimezoneOrDefault()),
		"controller_timezone": cfg.TimezoneOrDefault(),
	}
}

// ConfiguredTime returns now in the configured time zone. The process keeps
// UTC as its local zone, so time.Now alone would report UTC.
func ConfiguredTime(now time.Time, timezone string) time.Time {
	return configuredTime(now, timezone)
}

// controllerTimeState renders the controller clock in the configured time
// zone. It rides on every user turn's state message, so the model always sees
// the current date and time without a date in the cached system prompt.
func controllerTimeState(now time.Time, timezone string) string {
	local := configuredTime(now, timezone)
	return local.Format("2006-01-02 15:04:05 -07:00") + " (" + local.Weekday().String() + ")"
}
