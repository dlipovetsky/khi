#!/bin/bash
# Copyright 2026 Google LLC
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


mkdir -p vendor
cd vendor
if [[ ! -d woff2 ]]; then
  git clone --recursive https://github.com/google/woff2.git
fi
cd woff2
# On macOS, set SDK and C++ include path so the compiler finds standard library headers (<map>, etc.)
if [[ "$(uname)" == "Darwin" ]]; then
  SDK_PATH="$(xcrun --sdk macosx --show-sdk-path)"
  # C++ standard library headers are in the SDK under usr/include/c++/v1
  CXX_INCLUDE="${SDK_PATH}/usr/include/c++/v1"
  export CXX="clang++"
  export CXXFLAGS="-isysroot ${SDK_PATH} -std=c++11 -I${CXX_INCLUDE}"
  export LDFLAGS="-isysroot ${SDK_PATH}"
fi
make clean all
cd ../..

./vendor/woff2/woff2_decompress ./node_modules/@fontsource/roboto/files/roboto-latin-700-normal.woff2
./vendor/woff2/woff2_decompress ./node_modules/material-symbols/material-symbols-outlined.woff2