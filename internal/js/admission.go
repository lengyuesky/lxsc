package js

import "lxsc/internal/admission"

// 所有 SDK 和音源共用名额，避免脚本数量增加后绕过全局上限。
var callLimits = admission.New(64, 128, 0, 0)
var fetchLimits = admission.New(8, 32, 0, 0)

func WorkloadStats() map[string]admission.Stats {
	return map[string]admission.Stats{"calls": callLimits.Stats(), "http": fetchLimits.Stats()}
}
