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

// Package requestcontrol defines the Director component responsible for orchestrating request processing after initial
// parsing.
package requestcontrol

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/gateway-api-inference-extension/api/v1alpha2"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/backend"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/datastore"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/handlers"

	// Assuming the predictor is located here. Adjust the import path if necessary.
	latencypredictor "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/latencypredictorasync"
	"sigs.k8s.io/gateway-api-inference-extension/pkg/epp/metrics"
	schedulingtypes "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/scheduling/types"
	errutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/error"
	logutil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/logging"
	requtil "sigs.k8s.io/gateway-api-inference-extension/pkg/epp/util/request"
)

/*
NOTE: To support this refined logic, the `handlers.RequestContext` struct
(defined in a different package) would need to be updated as follows:

type RequestContext struct {
    // ... existing fields ...
	RequestReceivedTimestamp time.Time
	FirstTokenTimestamp      time.Time
	ResponseCompleteTimestamp time.Time
	IsModelServerStreaming   func() bool
	ResponseComplete         bool
	Prompt                   string
	LastSeenMetrics           *backend.Metrics
    // ... etc ...

    // -- New fields for latency predictor --
    PredictedTTFT                float64   // The predicted TTFT in milliseconds
    PredictedTPOT                float64   // The predicted TPOT in milliseconds
    TTFT                         float64   // Actual Time To First Token in milliseconds
    LastTokenTimestamp           time.Time // Timestamp of the last token received
    TPOTObservations            []float64  // All actual inter-token latencies (for which we have predictions)
    PredictedTPOTObservations   []float64  // Predicted inter-token latencies (only for sampled tokens)
    GeneratedTokenCount          int       // Current number of tokens generated
}

*/

const (
	// Poisson sampling parameters for predictions
	defaultSamplingMean = 50 // Mean interval between prediction samples (tokens)
	maxSampledTokens    = 50   // Maximum number of prediction samples per request
)

// splitWords splits a string into words based on whitespace and returns the resulting slice.
func splitWords(input string) []string {
	return strings.Fields(input)
}


// calculateRunningAverage calculates the running average efficiently
func calculateRunningAverage(currentAvg float64, newValue float64, count int) float64 {
	if count == 0 {
		return 0
	}
	if count == 1 {
		return newValue
	}
	return currentAvg + (newValue-currentAvg)/float64(count)
}

// Scheduler defines the interface required by the Director for scheduling.
type Scheduler interface {
	Schedule(ctx context.Context, b *schedulingtypes.LLMRequest) (result *schedulingtypes.SchedulingResult, err error)
}

// SaturationDetector provides a signal indicating whether the backends are considered saturated.
type SaturationDetector interface {
	IsSaturated(ctx context.Context) bool
}

func NewDirectorWithConfig(datastore datastore.Datastore, scheduler Scheduler, saturationDetector SaturationDetector, config *Config, predictor latencypredictor.PredictorInterface) *Director {
	log.Log.Info("Director created", 
		"predictor", predictor, 
		"predictorIsNil", predictor == nil,
		"predictorType", fmt.Sprintf("%T", predictor))
	return &Director{
		datastore:           datastore,
		scheduler:           scheduler,
		saturationDetector:  saturationDetector,
		latencyPredictor:    predictor,
		preRequestPlugins:   config.preRequestPlugins,
		postResponsePlugins: config.postResponsePlugins,
	}
}

// Director orchestrates the request handling flow, including scheduling.
type Director struct {
	datastore           datastore.Datastore
	scheduler           Scheduler
	saturationDetector  SaturationDetector
	latencyPredictor    latencypredictor.PredictorInterface
	preRequestPlugins   []PreRequest
	postResponsePlugins []PostResponse
}

// HandleRequest orchestrates the request lifecycle.
func (d *Director) HandleRequest(ctx context.Context, reqCtx *handlers.RequestContext) (*handlers.RequestContext, error) {
	logger := log.FromContext(ctx)

	// --- 1. Parse Request Details ---
	var ok bool
	requestBodyMap := reqCtx.Request.Body
	reqCtx.Model, ok = requestBodyMap["model"].(string)
	if !ok {
		return reqCtx, errutil.Error{Code: errutil.BadRequest, Msg: "model not found in request body"}
	}
	prompt, err := requtil.ExtractPromptFromRequestBody(requestBodyMap)
	if err != nil {
		return reqCtx, err
	} else {
		reqCtx.Prompt = prompt
	}

	modelObj := d.datastore.ModelGet(reqCtx.Model)
	if modelObj == nil {
		logger.Info("No associated inferenceModel found, using default", "model", reqCtx.Model)
		sheddable := v1alpha2.Sheddable
		modelObj = &v1alpha2.InferenceModel{
			Spec: v1alpha2.InferenceModelSpec{
				ModelName:   reqCtx.Model,
				Criticality: &sheddable,
			},
		}
	}

	reqCtx.ResolvedTargetModel = reqCtx.Model
	if len(modelObj.Spec.TargetModels) > 0 {
		reqCtx.ResolvedTargetModel = RandomWeightedDraw(logger, modelObj, 0)
		if reqCtx.ResolvedTargetModel == "" {
			return reqCtx, errutil.Error{Code: errutil.BadConfiguration, Msg: fmt.Sprintf("error getting target model name for model %v", modelObj.Name)}
		}
		reqCtx.Request.Body["model"] = reqCtx.ResolvedTargetModel // Update target model in the body.
	}

	requestCriticality := v1alpha2.Standard
	if modelObj.Spec.Criticality != nil {
		requestCriticality = *modelObj.Spec.Criticality
	}

	// Prepare LLMRequest (needed for both saturation detection and Scheduler)
	reqCtx.SchedulingRequest = &schedulingtypes.LLMRequest{
		RequestId:   reqCtx.Request.Headers[requtil.RequestIdHeaderKey],
		TargetModel: reqCtx.ResolvedTargetModel,
		Prompt:      prompt,
		Headers:     reqCtx.Request.Headers,
	}

	logger = logger.WithValues("model", reqCtx.Model, "resolvedTargetModel", reqCtx.ResolvedTargetModel, "criticality", requestCriticality)
	ctx = log.IntoContext(ctx, logger)
	logger.V(logutil.DEBUG).Info("LLM request assembled")

	// --- 2. Admission Control check --
	if err := d.admitRequest(ctx, requestCriticality); err != nil {
		return reqCtx, err
	}

	// --- 3. Call Scheduler ---
	results, err := d.scheduler.Schedule(ctx, reqCtx.SchedulingRequest)
	if err != nil {
		return reqCtx, errutil.Error{Code: errutil.InferencePoolResourceExhausted, Msg: fmt.Errorf("failed to find target pod: %w", err).Error()}
	}

	// --- 4. Prepare Request ---
	reqCtx, err = d.prepareRequest(ctx, reqCtx, results)
	if err != nil {
		return reqCtx, err
	}

	return reqCtx, nil
}

// admitRequest handles admission control to decide whether or not to accept the request
// based on the request criticality and system saturation state.
func (d *Director) admitRequest(ctx context.Context, requestCriticality v1alpha2.Criticality) error {
	logger := log.FromContext(ctx)

	if requestCriticality == v1alpha2.Critical {
		logger.V(logutil.DEBUG).Info("Critical request bypassing saturation check.")
		return nil
	}

	logger.V(logutil.DEBUG).Info("Performing saturation check for non-critical request.")
	if d.saturationDetector.IsSaturated(ctx) {
		return errutil.Error{
			Code: errutil.InferencePoolResourceExhausted,
			Msg:  "system saturated, non-critical request dropped",
		}
	}

	return nil
}

// prepareRequest sets endpoint & optionally initializes LastSeenMetrics.
func (d *Director) prepareRequest(ctx context.Context, reqCtx *handlers.RequestContext, result *schedulingtypes.SchedulingResult) (*handlers.RequestContext, error) {
	if result == nil || len(result.ProfileResults) == 0 {
		return reqCtx, errutil.Error{Code: errutil.Internal, Msg: "empty scheduling results"}
	}

	pr, ok := result.ProfileResults[result.PrimaryProfileName]
	if ok && pr.TargetPod != nil {
		reqCtx.LastSeenMetrics = pr.TargetPod.GetMetrics().Clone()
	}

	// Always set endpoint even if metrics missing
	pod := pr.TargetPod.GetPod()
	pool, err := d.datastore.PoolGet()
	if err != nil {
		return reqCtx, err
	}
	reqCtx.TargetPod = pod
	reqCtx.TargetEndpoint = net.JoinHostPort(pod.Address, strconv.Itoa(int(pool.Spec.TargetPortNumber)))
	reqCtx.SchedulingResult = result
	d.runPreRequestPlugins(ctx, reqCtx.SchedulingRequest, result, int(pool.Spec.TargetPortNumber))
	return reqCtx, nil
}

// HandleResponseHeaders is called when the first chunk of the response arrives.
func (d *Director) HandleResponseHeaders(ctx context.Context, reqCtx *handlers.RequestContext) (*handlers.RequestContext, error) {
    logger := log.FromContext(ctx).WithValues("stage", "headers")
    logger.V(logutil.DEBUG).Info("Entering HandleResponseHeaders")

    response := &Response{
        RequestId: reqCtx.Request.Headers[requtil.RequestIdHeaderKey],
        Headers:   reqCtx.Response.Headers,
    }
    d.runPostResponsePlugins(ctx, reqCtx.SchedulingRequest, response, reqCtx.TargetPod)

    if d.latencyPredictor == nil {
        logger.V(logutil.DEBUG).Info("No latency predictor configured; skipping header prediction")
        return reqCtx, nil
    }
    if reqCtx.SchedulingResult == nil {
        logger.V(logutil.DEBUG).Info("No scheduling result; skipping header prediction")
        return reqCtx, nil
    }

    pr, ok := reqCtx.SchedulingResult.ProfileResults[reqCtx.SchedulingResult.PrimaryProfileName]
    if !ok || pr.TargetPod == nil {
        logger.V(logutil.DEBUG).Info("No target pod metrics; skipping header prediction", "primaryProfile", reqCtx.SchedulingResult.PrimaryProfileName)
        return reqCtx, nil
    }

    // Refresh metrics
    reqCtx.LastSeenMetrics = pr.TargetPod.GetMetrics().Clone()
    logger.V(logutil.DEBUG).Info("Refreshed LastSeenMetrics at header", 
        "KVCache%", reqCtx.LastSeenMetrics.KVCacheUsagePercent,
        "Waiting", reqCtx.LastSeenMetrics.WaitingQueueSize,
        "Running", reqCtx.LastSeenMetrics.RunningQueueSize,
    )

    // Build prediction request for TTFT
    predictionReq := latencypredictor.PredictionRequest{
        KVCachePercentage:  reqCtx.LastSeenMetrics.KVCacheUsagePercent,
        InputTokenLength:   len(splitWords(reqCtx.Prompt)),
        NumRequestWaiting:  reqCtx.LastSeenMetrics.WaitingQueueSize,
        NumRequestRunning:  reqCtx.LastSeenMetrics.RunningQueueSize,
        NumTokensGenerated: 0, // TTFT is for the first token
    }
    logger.V(logutil.DEBUG).Info("Header prediction request built", "req", predictionReq)

    // Always predict TTFT (not sampled since it's critical for scheduling decisions)
    if prediction, err := d.makePredictionSafely(ctx, predictionReq, "TTFT"); err != nil {
        logger.V(logutil.DEBUG).Error(err, "TTFT prediction failed")
        reqCtx.PredictedTTFT = 0 // Default to 0 on error
    } else {
        reqCtx.PredictedTTFT = prediction
        logger.V(logutil.DEBUG).Info("Predicted TTFT at header stage", 
            "predicted_ttft_ms", prediction)
    }

    logger.V(logutil.DEBUG).Info("Exiting HandleResponseHeaders")
    return reqCtx, nil
}

func (d *Director) HandleResponseBodyChunk(ctx context.Context, reqCtx *handlers.RequestContext) error {
    logger := log.FromContext(ctx).WithValues("stage", "bodyChunk")
    logger.V(logutil.DEBUG).Info("Entering HandleResponseBodyChunk")

    if d.latencyPredictor == nil || reqCtx.SchedulingResult == nil {
        logger.V(logutil.DEBUG).Info("Skipping body-chunk logic; predictor or scheduling missing")
        return nil
    }
    
    pr, ok := reqCtx.SchedulingResult.ProfileResults[reqCtx.SchedulingResult.PrimaryProfileName]
    if !ok || pr.TargetPod == nil {
        logger.V(logutil.DEBUG).Info("Skipping body-chunk logic; no valid target pod")
        return nil
    }

    now := time.Now()

    // Initialize per-request sampler on first call
    if reqCtx.TokenSampler == nil {
        requestID := reqCtx.Request.Headers[requtil.RequestIdHeaderKey]
        reqCtx.TokenSampler = requtil.NewTokenSampler(requestID, defaultSamplingMean, maxSampledTokens)
        logger.V(logutil.DEBUG).Info("Initialized per-request token sampler for predictions", 
            "first_prediction_token", reqCtx.TokenSampler.GetNextSampleToken(),
            "request_id", requestID)
    }


    // Determine if this is the first token
    isFirstToken := reqCtx.TTFT == 0

    if isFirstToken {
        // Calculate and record TTFT
        reqCtx.TTFT = float64(now.Sub(reqCtx.RequestReceivedTimestamp).Milliseconds())
        reqCtx.GeneratedTokenCount = 1
        
        logger.V(logutil.DEBUG).Info("First token received", "ttft_ms", reqCtx.TTFT)

        // ALWAYS add TTFT training data (no sampling for training)
        entry := latencypredictor.TrainingEntry{
            KVCachePercentage:  reqCtx.LastSeenMetrics.KVCacheUsagePercent,
            InputTokenLength:   len(splitWords(reqCtx.Prompt)),
            ActualTTFT:         reqCtx.TTFT,
            ActualTPOT:         0, // Not applicable for TTFT
            Timestamp:          now,
            NumRequestWaiting:  reqCtx.LastSeenMetrics.WaitingQueueSize,
            NumRequestRunning:  reqCtx.LastSeenMetrics.RunningQueueSize,
            NumTokensGenerated: 0, // TTFT is for the first token
        }
        
        if err := d.latencyPredictor.AddTrainingDataBulk([]latencypredictor.TrainingEntry{entry}); err != nil {
            logger.V(logutil.DEBUG).Error(err, "Failed to add TTFT training sample")
        } else {
            logger.V(logutil.DEBUG).Info("Successfully added TTFT training sample")
        }

		// ALWAYS predict the first TPOT using current metrics state
        // This predicts what the latency will be for the NEXT token (token 2)
        firstTPOTPredictionReq := latencypredictor.PredictionRequest{
            KVCachePercentage:  reqCtx.LastSeenMetrics.KVCacheUsagePercent,
            InputTokenLength:   len(splitWords(reqCtx.Prompt)),
            NumRequestWaiting:  reqCtx.LastSeenMetrics.WaitingQueueSize,
            NumRequestRunning:  reqCtx.LastSeenMetrics.RunningQueueSize,
            NumTokensGenerated: reqCtx.GeneratedTokenCount, // Currently 1, predicting for token 2
        }

        if prediction, err := d.makePredictionSafely(ctx, firstTPOTPredictionReq, "TPOT"); err != nil {
            logger.V(logutil.DEBUG).Error(err, "First TPOT prediction failed")
            reqCtx.PredictedTPOTObservations = append(reqCtx.PredictedTPOTObservations, 0)
            // Update average with 0 prediction
            reqCtx.AvgPredictedTPOT = calculateRunningAverage(reqCtx.AvgPredictedTPOT, 0, len(reqCtx.PredictedTPOTObservations))
        } else {
            reqCtx.PredictedTPOTObservations = append(reqCtx.PredictedTPOTObservations, prediction)
            reqCtx.AvgPredictedTPOT = calculateRunningAverage(reqCtx.AvgPredictedTPOT, prediction, len(reqCtx.PredictedTPOTObservations))
            logger.V(logutil.DEBUG).Info("Predicted first TPOT based on current metrics", 
                "predicted_first_tpot_ms", prediction,
                "kv_cache_percent", reqCtx.LastSeenMetrics.KVCacheUsagePercent,
                "waiting_queue", reqCtx.LastSeenMetrics.WaitingQueueSize,
                "running_queue", reqCtx.LastSeenMetrics.RunningQueueSize,
            )
        }

    } else {
        // Calculate inter-token latency (TPOT)
        interTokenLatency := float64(now.Sub(reqCtx.LastTokenTimestamp).Milliseconds())
        reqCtx.GeneratedTokenCount++

        //log the inter-token latency for predicted samples
         if reqCtx.GeneratedTokenCount == 2 || reqCtx.TokenSampler.ShouldPredict(reqCtx.GeneratedTokenCount) { //tricky logic, since next sample token is always +1 from current token
            reqCtx.TPOTObservations = append(reqCtx.TPOTObservations, interTokenLatency)
            reqCtx.AvgTPOT = calculateRunningAverage(reqCtx.AvgTPOT, interTokenLatency, len(reqCtx.TPOTObservations))
        }

        
        
        // ALWAYS record actual TPOT for training (store ALL observations)
       
        
        logger.V(logutil.DEBUG).Info("Inter-token latency measured", 
            "latency_ms", interTokenLatency,
            "token_count", reqCtx.GeneratedTokenCount,
            "total_sampled_observations", len(reqCtx.TPOTObservations),
            "next_prediction_token", reqCtx.TokenSampler.GetNextSampleToken(),
            
        )

        // ALWAYS add training data (every token contributes to learning)
        trainingEntry := latencypredictor.TrainingEntry{
            KVCachePercentage:  reqCtx.LastSeenMetrics.KVCacheUsagePercent,
            InputTokenLength:   len(splitWords(reqCtx.Prompt)),
            ActualTTFT:         0, // Not applicable for TPOT
            ActualTPOT:         interTokenLatency,
            Timestamp:          now,
            NumRequestWaiting:  reqCtx.LastSeenMetrics.WaitingQueueSize,
            NumRequestRunning:  reqCtx.LastSeenMetrics.RunningQueueSize,
            NumTokensGenerated: reqCtx.GeneratedTokenCount - 1, // Current token count
        }

        if err := d.latencyPredictor.AddTrainingDataBulk([]latencypredictor.TrainingEntry{trainingEntry}); err != nil {
            logger.V(logutil.DEBUG).Error(err, "Failed to add TPOT training sample")
        } else {
            logger.V(logutil.DEBUG).Info("Successfully added TPOT training sample", 
                "token_count", reqCtx.GeneratedTokenCount,
                "total_predicting_samples", len(reqCtx.TPOTObservations))
        }

        // Only make predictions for SAMPLED tokens (to reduce overhead)
        if reqCtx.TokenSampler.ShouldPredict(reqCtx.GeneratedTokenCount) {
            logger.V(logutil.DEBUG).Info("Making TPOT prediction for sampled token", 
                "token_count", reqCtx.GeneratedTokenCount,
                "prediction_number", reqCtx.TokenSampler.GetSampleCount()+1,
            )

            // Make TPOT prediction for next sampled token
            predictionReq := latencypredictor.PredictionRequest{
                KVCachePercentage:  reqCtx.LastSeenMetrics.KVCacheUsagePercent,
                InputTokenLength:   len(splitWords(reqCtx.Prompt)),
                NumRequestWaiting:  reqCtx.LastSeenMetrics.WaitingQueueSize,
                NumRequestRunning:  reqCtx.LastSeenMetrics.RunningQueueSize,
                NumTokensGenerated: reqCtx.GeneratedTokenCount, // Current token count
            }

            if prediction, err := d.makePredictionSafely(ctx, predictionReq, "TPOT"); err != nil {
                logger.V(logutil.DEBUG).Error(err, "TPOT prediction failed", "token", reqCtx.GeneratedTokenCount)
                reqCtx.PredictedTPOTObservations = append(reqCtx.PredictedTPOTObservations, 0)
                // Update average with 0 prediction
                reqCtx.AvgPredictedTPOT = calculateRunningAverage(reqCtx.AvgPredictedTPOT, 0, len(reqCtx.PredictedTPOTObservations))
            } else {
                reqCtx.PredictedTPOTObservations = append(reqCtx.PredictedTPOTObservations, prediction)
                reqCtx.AvgPredictedTPOT = calculateRunningAverage(reqCtx.AvgPredictedTPOT, prediction, len(reqCtx.PredictedTPOTObservations))
                logger.V(logutil.DEBUG).Info("Predicted TPOT for sampled token", 
                    "predicted_tpot_ms", prediction,
                    "token", reqCtx.GeneratedTokenCount,
                    "avg_tpot_ms", reqCtx.AvgTPOT,
                    "sampled_tokens", len(reqCtx.PredictedTPOTObservations),
                )
            }

            // Record the prediction and calculate next sample token
            reqCtx.TokenSampler.RecordPrediction(reqCtx.GeneratedTokenCount)
            
            if reqCtx.TokenSampler.GetSampleCount() < maxSampledTokens {
                logger.V(logutil.DEBUG).Info("Scheduled next prediction", 
                    "current_token", reqCtx.GeneratedTokenCount,
                    "next_prediction_token", reqCtx.TokenSampler.GetNextSampleToken(),
                )
            } else {
                logger.V(logutil.DEBUG).Info("Reached maximum predictions, no more predictions", 
                    "max_predictions", maxSampledTokens)
            }
        } else {
            logger.V(logutil.DEBUG).Info("Skipping prediction for this token (training still performed)", 
                "token_count", reqCtx.GeneratedTokenCount,
                "next_prediction_token", reqCtx.TokenSampler.GetNextSampleToken(),
                "predictions_made", reqCtx.TokenSampler.GetSampleCount(),
            )
        }

        
    }
    // Always update timestamp for next calculation
        reqCtx.LastTokenTimestamp = now
        // Refresh metrics
    reqCtx.LastSeenMetrics = pr.TargetPod.GetMetrics().Clone()
    logger.V(logutil.DEBUG).Info("Refreshed LastSeenMetrics at body chunk", 
        "KVCache%", reqCtx.LastSeenMetrics.KVCacheUsagePercent,
        "Waiting", reqCtx.LastSeenMetrics.WaitingQueueSize,
        "Running", reqCtx.LastSeenMetrics.RunningQueueSize,
    )

    logger.V(logutil.DEBUG).Info("Exiting HandleResponseBodyChunk")
    return nil
}

func (d *Director) makePredictionSafely(ctx context.Context, req latencypredictor.PredictionRequest, predictionType string) (float64, error) {
    // Validate input
    if req.InputTokenLength < 0 {
        return 0, fmt.Errorf("invalid prediction request: negative token counts")
    }
    
    start := time.Now()
    prediction, err := d.latencyPredictor.Predict(ctx, req)
    duration := time.Since(start)
    
    if err != nil {
        log.FromContext(ctx).V(logutil.DEBUG).Error(err, 
            "Prediction failed", 
            "type", predictionType,
            "duration", duration,
        )
        return 0, err
    }
    
    if prediction == nil {
        return 0, fmt.Errorf("predictor returned nil prediction")
    }
    
    var result float64
    switch predictionType {
    case "TTFT":
        result = prediction.TTFT
    case "TPOT":
        result = prediction.TPOT
    default:
        return 0, fmt.Errorf("unknown prediction type: %s", predictionType)
    }
    
    // Validate result
    if result < 0 {
        log.FromContext(ctx).V(logutil.DEBUG).Info("Negative prediction received", 
            "type", predictionType, 
            "value", result,
        )
        return 0, nil // Return 0 for negative predictions
    }
    
    log.FromContext(ctx).V(logutil.DEBUG).Info("Prediction successful", 
        "type", predictionType,
        "value", result,
        "duration", duration,
    )
    
    return result, nil
}

// HandleResponseTrailers calculates final aggregate metrics and adds them to response trailers.
func (d *Director) HandleResponseTrailers(ctx context.Context, reqCtx *handlers.RequestContext) (*handlers.RequestContext, error) {
    logger := log.FromContext(ctx).WithValues("stage", "trailers")
    logger.V(logutil.DEBUG).Info("Entering HandleResponseTrailers")
    return reqCtx, nil
}

func (d *Director) GetRandomPod() *backend.Pod {
	pods := d.datastore.PodGetAll()
	if len(pods) == 0 {
		return nil
	}
	number := rand.Intn(len(pods))
	pod := pods[number]
	return pod.GetPod()
}

func RandomWeightedDraw(logger logr.Logger, model *v1alpha2.InferenceModel, seed int64) string {
	// TODO: after we are down to 1 server implementation, make these methods a part of the struct
	// and handle random seeding on the struct.
	source := rand.NewSource(rand.Int63())
	if seed > 0 {
		source = rand.NewSource(seed)
	}
	r := rand.New(source)

	// all the weight values are nil, then we should return random model name
	if model.Spec.TargetModels[0].Weight == nil {
		index := r.Int31n(int32(len(model.Spec.TargetModels)))
		return model.Spec.TargetModels[index].Name
	}

	var weights int32
	for _, model := range model.Spec.TargetModels {
		weights += *model.Weight
	}
	logger.V(logutil.DEBUG).Info("Weights for model computed", "model", model.Name, "weights", weights)
	randomVal := r.Int31n(weights)
	// TODO: optimize this without using loop
	for _, model := range model.Spec.TargetModels {
		if randomVal < *model.Weight {
			return model.Name
		}
		randomVal -= *model.Weight
	}
	return ""
}

func (d *Director) runPreRequestPlugins(ctx context.Context, request *schedulingtypes.LLMRequest, schedulingResult *schedulingtypes.SchedulingResult,
	targetPort int) {
	for _, plugin := range d.preRequestPlugins {
		log.FromContext(ctx).V(logutil.DEBUG).Info("Running pre-request plugin", "plugin", plugin.Name())
		before := time.Now()
		plugin.PreRequest(ctx, request, schedulingResult, targetPort)
		metrics.RecordRequestControlPluginProcessingLatency(PreRequestPluginType, plugin.Name(), time.Since(before))
	}
}

func (d *Director) runPostResponsePlugins(ctx context.Context, request *schedulingtypes.LLMRequest, response *Response, targetPod *backend.Pod) {
	for _, plugin := range d.postResponsePlugins {
		log.FromContext(ctx).V(logutil.DEBUG).Info("Running post-response plugin", "plugin", plugin.Name())
		before := time.Now()
		plugin.PostResponse(ctx, request, response, targetPod)
		metrics.RecordRequestControlPluginProcessingLatency(PostResponsePluginType, plugin.Name(), time.Since(before))
	}
}

func (d *Director) IsPredictorAvailable() bool {
    return d.latencyPredictor != nil
}