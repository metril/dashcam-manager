package media

import "testing"

func TestDevicesFor(t *testing.T) {
	cases := []struct {
		name string
		devs Devices
		mode Mode
		want string
	}{
		{"nvenc uses CUDA", Devices{DRI: "/dev/dri/renderD128", CUDA: "1"}, ModeNVENC, "1"},
		{"nvenc empty CUDA defaults to 0", Devices{DRI: "/dev/dri/renderD128"}, ModeNVENC, "0"},
		{"qsv uses DRI", Devices{DRI: "/dev/dri/renderD128", CUDA: "1"}, ModeQSV, "/dev/dri/renderD128"},
		{"vaapi uses DRI", Devices{DRI: "/dev/dri/renderD128", CUDA: "1"}, ModeVAAPI, "/dev/dri/renderD128"},
		{"software uses DRI", Devices{DRI: "/dev/dri/renderD128"}, ModeSoftware, "/dev/dri/renderD128"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.devs.For(tc.mode); got != tc.want {
				t.Errorf("Devices.For(%s) = %q, want %q", tc.mode, got, tc.want)
			}
		})
	}
}
