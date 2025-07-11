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

package handlers

import (
	"context"
	"encoding/json"
	"strings"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	filterPb "github.com/envoyproxy/go-control-plane/envoy/extensions/filters/http/ext_proc/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	"github.com/go-logr/logr"

	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/metrics"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
)

const (
	streamingRespPrefix = "data: "
	streamingEndMsg     = "data: [DONE]"
)

// HandleResponseBody always returns the requestContext even in the error case, as the request context is used in error handling.
func (s *StreamingServer) HandleResponseBody(
	ctx context.Context,
	reqCtx *RequestContext,
	response map[string]interface{},
) (*RequestContext, error) {
	logger := log.FromContext(ctx)
	responseBytes, err := json.Marshal(response)
	if err != nil {
		logger.V(logutil.DEFAULT).Error(err, "error marshalling responseBody")
		return reqCtx, err
	}
	if response["usage"] != nil {
		usg := response["usage"].(map[string]interface{})
		usage := Usage{
			PromptTokens:     int(usg["prompt_tokens"].(float64)),
			CompletionTokens: int(usg["completion_tokens"].(float64)),
			TotalTokens:      int(usg["total_tokens"].(float64)),
		}
		reqCtx.Usage = usage
		logger.V(logutil.VERBOSE).Info("Response generated", "usage", reqCtx.Usage)
	}
	reqCtx.ResponseSize = len(responseBytes)
	// ResponseComplete is to indicate the response is complete. In non-streaming
	// case, it will be set to be true once the response is processed; in
	// streaming case, it will be set to be true once the last chunk is processed.
	// TODO(https://github.com/kubernetes-sigs/gateway-api-inference-extension/issues/178)
	// will add the processing for streaming case.
	reqCtx.ResponseComplete = true

	reqCtx.respBodyResp = generateResponseBodyResponses(responseBytes, true, reqCtx, logger)
	return reqCtx, nil
}

// The function is to handle streaming response if the modelServer is streaming.
func (s *StreamingServer) HandleResponseBodyModelStreaming(
	ctx context.Context,
	reqCtx *RequestContext,
	responseText string,
) {
	if strings.Contains(responseText, streamingEndMsg) {
		resp := parseRespForUsage(ctx, responseText)
		reqCtx.Usage = resp.Usage
		metrics.RecordInputTokens(reqCtx.Model, reqCtx.ResolvedTargetModel, resp.Usage.PromptTokens)
		metrics.RecordOutputTokens(reqCtx.Model, reqCtx.ResolvedTargetModel, resp.Usage.CompletionTokens)
	}
	s.director.HandleResponseBodyChunk(ctx, reqCtx)
}

// The function is to handle streaming response if the modelServer is streaming.
func (s *StreamingServer) HandleResponseTrailers(
	ctx context.Context,
	reqCtx *RequestContext,
) (*RequestContext, error) {

	return s.director.HandleResponseTrailers(ctx, reqCtx)
}

func (s *StreamingServer) HandleResponseHeaders(ctx context.Context, reqCtx *RequestContext, resp *extProcPb.ProcessingRequest_ResponseHeaders) (*RequestContext, error) {
	for _, header := range resp.ResponseHeaders.Headers.Headers {
		if header.RawValue != nil {
			reqCtx.Response.Headers[header.Key] = string(header.RawValue)
		} else {
			reqCtx.Response.Headers[header.Key] = header.Value
		}
	}

	reqCtx, err := s.director.HandleResponseHeaders(ctx, reqCtx)

	return reqCtx, err
}

func (s *StreamingServer) generateResponseHeaderResponse(reqCtx *RequestContext) *extProcPb.ProcessingResponse {
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extProcPb.HeadersResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: s.generateResponseHeaders(reqCtx),
					},
				},
			},
		},
		ModeOverride: &filterPb.ProcessingMode{
			ResponseTrailerMode: filterPb.ProcessingMode_SEND,
		},
	}
}

// generateResponseTrailerResponse generates a response for trailers.
func (s *StreamingServer) generateResponseTrailerResponse(reqCtx *RequestContext) *extProcPb.ProcessingResponse {
	return &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseTrailers{
			ResponseTrailers: &extProcPb.TrailersResponse{
				HeaderMutation: &extProcPb.HeaderMutation{
					// Correct field or remove if unnecessary
					SetHeaders: s.generateResponseTrailers(reqCtx),
				},
			},
		},
	}
}

func generateResponseBodyResponses(
	responseBodyBytes []byte,
	setEoS bool,
	reqCtx *RequestContext,
	logger logr.Logger,
) []*extProcPb.ProcessingResponse {
	if reqCtx != nil && reqCtx.ModelServerStreaming {

		raw := string(responseBodyBytes)
		events := strings.Split(raw, "\n\n")

		var rebuilt strings.Builder
		for _, ev := range events {
			if !strings.HasPrefix(ev, "data: ") {
				continue
			}
			payload := strings.TrimPrefix(ev, "data: ")
			if payload == "[DONE]" {
				rebuilt.WriteString("data: [DONE]\n\n")
				continue
			}

			// Try to unmarshal only the JSON
			var obj map[string]interface{}
			if err := json.Unmarshal([]byte(payload), &obj); err != nil {
				logger.Error(err, "failed to unmarshal SSE payload", "payload", payload)
			} else {
				if usage, ok := obj["usage"].(map[string]interface{}); ok && usage != nil {
					usage["ttft_ms"] = reqCtx.TTFT
					usage["predicted_ttft_ms"] = reqCtx.PredictedTTFT
					usage["tpot_observations_ms"] = reqCtx.TPOTObservations
					usage["predicted_tpot_observations_ms"] = reqCtx.PredictedTPOTObservations
					usage["avg_tpot_ms"] = reqCtx.AvgTPOT
					usage["avg_predicted_tpot_ms"] = reqCtx.AvgPredictedTPOT
				}
				if mod, err := json.Marshal(obj); err != nil {
					logger.Error(err, "failed to re-marshal modified JSON", "obj", obj)
				} else {
					payload = string(mod)
				}
			}

			// Re-attach SSE prefix
			rebuilt.WriteString("data: ")
			rebuilt.WriteString(payload)
			rebuilt.WriteString("\n\n")
		}

		// Feed into your existing chunker
		modified := []byte(rebuilt.String())
		commonResponses := buildCommonResponses(modified, bodyByteLimit, setEoS)

		// Wrap as ProcessingResponses
		out := make([]*extProcPb.ProcessingResponse, 0, len(commonResponses))
		for _, cr := range commonResponses {
			out = append(out, &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_ResponseBody{
					ResponseBody: &extProcPb.BodyResponse{
						Response: cr,
					},
				},
			})
		}
		return out
	} else {
		commonResponses := buildCommonResponses(responseBodyBytes, bodyByteLimit, setEoS)
		responses := []*extProcPb.ProcessingResponse{}
		for _, commonResp := range commonResponses {
			resp := &extProcPb.ProcessingResponse{
				Response: &extProcPb.ProcessingResponse_ResponseBody{
					ResponseBody: &extProcPb.BodyResponse{
						Response: commonResp,
					},
				},
			}
			responses = append(responses, resp)
		}
		return responses
	}

}

func (s *StreamingServer) generateResponseHeaders(reqCtx *RequestContext) []*configPb.HeaderValueOption {
	// can likely refactor these two bespoke headers to be updated in PostDispatch, to centralize logic.
	headers := []*configPb.HeaderValueOption{
		{
			Header: &configPb.HeaderValue{
				// This is for debugging purpose only.
				Key:      "x-went-into-resp-headers",
				RawValue: []byte("true"),
			},
		},
	}

	// include all headers
	for key, value := range reqCtx.Response.Headers {
		headers = append(headers, &configPb.HeaderValueOption{
			Header: &configPb.HeaderValue{
				Key:      key,
				RawValue: []byte(value),
			},
		})
	}
	return headers
}

func (s *StreamingServer) generateResponseTrailers(reqCtx *RequestContext) []*configPb.HeaderValueOption {
	// can likely refactor these two bespoke headers to be updated in PostDispatch, to centralize logic.
	trailers := []*configPb.HeaderValueOption{
		{
			Header: &configPb.HeaderValue{
				// This is for debugging purpose only.
				Key:      "x-went-into-resp-trailers",
				RawValue: []byte("true"),
			},
		},
	}

	// include all headers
	for key, value := range reqCtx.Response.Trailers {
		trailers = append(trailers, &configPb.HeaderValueOption{
			Header: &configPb.HeaderValue{
				Key:      key,
				RawValue: []byte(value),
			},
		})
	}
	return trailers
}

// Example message if "stream_options": {"include_usage": "true"} is included in the request:
// data: {"id":"...","object":"text_completion","created":1739400043,"model":"food-review-0","choices":[],
// "usage":{"prompt_tokens":7,"total_tokens":17,"completion_tokens":10}}
//
// data: [DONE]
//
// Noticed that vLLM returns two entries in one response.
// We need to strip the `data:` prefix and next Data: [DONE] from the message to fetch response data.
//
// If include_usage is not included in the request, `data: [DONE]` is returned separately, which
// indicates end of streaming.
func parseRespForUsage(
	ctx context.Context,
	responseText string,
) ResponseBody {
	response := ResponseBody{}
	logger := log.FromContext(ctx)

	lines := strings.Split(responseText, "\n")
	for _, line := range lines {
		if !strings.HasPrefix(line, streamingRespPrefix) {
			continue
		}
		content := strings.TrimPrefix(line, streamingRespPrefix)
		if content == "[DONE]" {
			continue
		}

		byteSlice := []byte(content)
		if err := json.Unmarshal(byteSlice, &response); err != nil {
			logger.Error(err, "unmarshaling response body")
			continue
		}
	}

	return response
}

type ResponseBody struct {
	Usage Usage `json:"usage"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
