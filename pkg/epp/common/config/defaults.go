/*
Copyright 2025 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package config holds common configuration default values used across
// different EPP components.
package config

const (
	// DefaultKVCacheThreshold is the default KV cache utilization (0.0 to 1.0)
	// threshold.
	DefaultKVCacheThreshold = 0.8
	// DefaultQueueThresholdCritical is the default backend waiting queue size
	// threshold.
	DefaultQueueThresholdCritical = 5

	// DefaultScorerWeight is the weight used for scorers referenced in the
	// configuration without explicit weights.
	DefaultScorerWeight = 1
)
