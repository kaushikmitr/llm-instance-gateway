#!/bin/bash

# Copyright 2024 Google Inc. All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.
set -o xtrace

export IP=$IP

huggingface-cli login --token "$HF_TOKEN" --add-to-git-credential

if [[ "$PROMPT_DATASET" = "sharegpt" ]]; then
  PROMPT_DATASET_FILE="ShareGPT_V3_unfiltered_cleaned_split.json"
fi

PYTHON="python3"
PYTHON_OPTS="qps_test.py"

# Define the arguments for a single run with request rate list and prompt count
timestamp=$(date +"%Y-%m-%d_%H-%M-%S")
output_file="latency-profile-${timestamp}.txt"
PYTHON_OPTS="$PYTHON_OPTS --save-json-results --host=$IP --port=$PORT --dataset=$PROMPT_DATASET_FILE --tokenizer=$TOKENIZER --request-rates=$REQUEST_RATES --num-prompts-values=$NUM_PROMPTS_VALUES  --backend=$BACKEND --max-input-length=$INPUT_LENGTH --max-output-length=$OUTPUT_LENGTH --file-prefix=$FILE_PREFIX --models=$MODELS"

# Optional flags
if [[ "$SCRAPE_SERVER_METRICS" = "true" ]]; then
    PYTHON_OPTS="$PYTHON_OPTS --scrape-server-metrics"
fi
if [[ "$SAVE_AGGREGATED_RESULT" = "true" ]]; then
    PYTHON_OPTS="$PYTHON_OPTS --save-aggregated-result"
fi

# Run the benchmark and save output to a file
$PYTHON $PYTHON_OPTS > $output_file
cat $output_file

# Indicate completion
export LPG_FINISHED="true"
sleep infinity
