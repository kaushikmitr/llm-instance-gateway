package handlers

import (
	"strconv"

	configPb "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	extProcPb "github.com/envoyproxy/go-control-plane/envoy/service/ext_proc/v3"
	klog "k8s.io/klog/v2"
)

// HandleResponseHeaders processes response headers from the backend model server.
func (s *Server) HandleResponseHeaders(reqCtx *RequestContext, req *extProcPb.ProcessingRequest) (*extProcPb.ProcessingResponse, error) {
	klog.V(3).Info("Processing ResponseHeaders")
	h := req.Request.(*extProcPb.ProcessingRequest_ResponseHeaders)
	klog.V(3).Infof("Headers before: %+v\n", h)

	var targetPodAddress string
	if reqCtx.TargetPod != nil {
		targetPodAddress = reqCtx.TargetPod.Address
	} else {
		targetPodAddress = "unknown"
	}

	resp := &extProcPb.ProcessingResponse{
		Response: &extProcPb.ProcessingResponse_ResponseHeaders{
			ResponseHeaders: &extProcPb.HeadersResponse{
				Response: &extProcPb.CommonResponse{
					HeaderMutation: &extProcPb.HeaderMutation{
						SetHeaders: []*configPb.HeaderValueOption{
							{
								Header: &configPb.HeaderValue{
									// This is for debugging purpose only.
									Key:      "x-went-into-resp-headers",
									RawValue: []byte("true"),
								},
							},
							{
								Header: &configPb.HeaderValue{
									Key:      "x-target-pod",
									RawValue: []byte(targetPodAddress),
								},
							},
							{
								Header: &configPb.HeaderValue{
									Key:      "x-kvcache-size-at-start",
									RawValue: []byte(strconv.FormatFloat(reqCtx.KVCacheSizeAtStart, 'f', -1, 64)),
								},
							},
							{
								Header: &configPb.HeaderValue{
									// This is for debugging purpose only.
									Key:      "x-waiting-queue-size-at-start",
									RawValue: []byte(strconv.Itoa(reqCtx.WaitingQueueAtStart)),
								},
							},
							{
								Header: &configPb.HeaderValue{
									// This is for debugging purpose only.
									Key:      "x-running-queue-size-at-start",
									RawValue: []byte(strconv.Itoa(reqCtx.RunningQueueAtStart)),
								},
							},
						},
					},
				},
			},
		},
	}
	return resp, nil
}
