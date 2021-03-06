/*
Copyright 2018 BlackRock, Inc.

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

package resource

import (
	"context"
	"fmt"

	"github.com/argoproj/argo-events/gateways"
	gwcommon "github.com/argoproj/argo-events/gateways/common"
)

// ValidateEventSource validates gateway event source
func (ese *ResourceEventSourceExecutor) ValidateEventSource(ctx context.Context, es *gateways.EventSource) (*gateways.ValidEventSource, error) {
	return gwcommon.ValidateGatewayEventSource(es.Data, parseEventSource, validateResource)
}

func validateResource(config interface{}) error {
	res := config.(*resource)
	if res == nil {
		return gwcommon.ErrNilEventSource
	}
	if res.Version == "" {
		return fmt.Errorf("resource version must be specified")
	}
	if res.Namespace == "" {
		return fmt.Errorf("resource namespace must be specified")
	}
	if res.Kind == "" {
		return fmt.Errorf("resource kind must be specified")
	}
	if res.Group == "" {
		return fmt.Errorf("resource group must be specified")
	}
	return nil
}
