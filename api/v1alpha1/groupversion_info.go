// Copyright 2026 Zyvor AI Labs · https://zyvor.dev
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 contains the AddressPool CRD's Go types (group
// rivora.zyvor.dev, matching the product's API group from its original
// design doc). rivora-controller accesses AddressPool via
// k8s.io/client-go/dynamic (no generated clientset — a single small CRD
// doesn't need client-gen's machinery), converting to/from these types
// with runtime's unstructured converter; GroupVersion/SchemeGroupVersion
// here are what that conversion and the CRD YAML both key off.
package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	GroupName = "rivora.zyvor.dev"
	Version   = "v1alpha1"
)

// GroupVersion is the API group/version AddressPool lives at.
var GroupVersion = schema.GroupVersion{Group: GroupName, Version: Version}

// AddressPoolResource is the GroupVersionResource the dynamic client uses
// to address AddressPool objects.
var AddressPoolResource = GroupVersion.WithResource("addresspools")

// AddressPoolKind is the GroupVersionKind for AddressPool.
var AddressPoolKind = GroupVersion.WithKind("AddressPool")
