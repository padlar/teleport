/*
Copyright 2023 Gravitational, Inc.

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

package externalauditstorage

import (
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gravitational/trace"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/api/types/header"
	"github.com/gravitational/teleport/api/types/header/convert/legacy"
	"github.com/gravitational/teleport/api/utils"
	"github.com/gravitational/teleport/api/utils/aws"
)

const (
	externalAuditStoragePolicyNamePrefix      = "ExternalAuditStoragePolicy-"
	externalAuditStorageLongtermBucketPrefix  = "s3://teleport-longterm-"
	externalAuditStorageTransientBucketPrefix = "s3://teleport-transient-"
)

// ExternalAuditStorage is internal representation of an External Audit Storage resource.
// Proto definion can be found https://github.com/gravitational/teleport/blob/master/api/proto/teleport/externalauditstorage/v1/externalauditstorage.proto
type ExternalAuditStorage struct {
	// ResourceHeader is the common resource header for all resources.
	header.ResourceHeader

	// Spec is the specification for the External Audit Storage.
	Spec ExternalAuditStorageSpec `json:"spec" yaml:"spec"`
}

// ExternalAuditStorageSpec is the specification for an External Audit Storage.
type ExternalAuditStorageSpec struct {
	// IntegrationName is name of existing OIDC integration used to
	// generate AWS credentials.
	IntegrationName string `json:"integration_name" yaml:"integration_name"`
	// PolicyName is the name of the IAM policy to attach to the integration
	// IAM role.
	PolicyName string `json:"policy_name" yaml:"policy_name"`
	// Region is the AWS region where the infrastructure is hosted.
	Region string `json:"region" yaml:"region"`
	// SessionRecordingsURI is s3 path used to store session recordings.
	SessionRecordingsURI string `json:"session_recordings_uri" yaml:"session_recordings_uri"`
	// AthenaWorkgroup is workgroup used by Athena audit logs during queries.
	AthenaWorkgroup string `json:"athena_workgroup" yaml:"athena_workgroup"`
	// GlueDatabase is database used by Athena audit logs during queries.
	GlueDatabase string `json:"glue_database" yaml:"glue_database"`
	// GlueTable is table used by Athena audit logs during queries.
	GlueTable string `json:"glue_table" yaml:"glue_table"`
	// AuditEventsLongTermURI is s3 path used to store batched parquet files with
	// audit events, partitioned by event date.
	AuditEventsLongTermURI string `json:"audit_events_long_term_uri" yaml:"audit_events_long_term_uri"`
	// AthenaResultsURI is s3 path used to store temporary results generated by
	// Athena engine.
	AthenaResultsURI string `json:"athena_results_uri" yaml:"athena_results_uri"`
}

// NewDraftExternalAuditStorage will create a new draft External Audit Storage.
func NewDraftExternalAuditStorage(metadata header.Metadata, spec ExternalAuditStorageSpec) (*ExternalAuditStorage, error) {
	externalaudit := &ExternalAuditStorage{
		ResourceHeader: header.ResourceHeaderFromMetadata(metadata),
		Spec:           spec,
	}

	name := externalaudit.GetName()
	switch {
	case name == "":
		externalaudit.SetName(types.MetaNameExternalAuditStorageDraft)
	case name != types.MetaNameExternalAuditStorageDraft:
		return nil, trace.BadParameter("draft External Audit Storage invalid name")
	}

	if err := externalaudit.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	return externalaudit, nil
}

// GenerateDraftExternalAuditStorage creates a new draft ExternalAuditStorage with
// randomized resource names.
func GenerateDraftExternalAuditStorage(integrationName, region string) (*ExternalAuditStorage, error) {
	// S3 bucket names can't use underscores, Glue tables can't use hyphens,
	// Athena workgroups can use either.
	// https://docs.aws.amazon.com/AmazonS3/latest/userguide/bucketnamingrules.html
	// https://docs.aws.amazon.com/athena/latest/ug/tables-databases-columns-names.html
	// https://docs.aws.amazon.com/athena/latest/ug/workgroups-settings.html
	nonce := uuid.NewString()
	underscoreNonce := strings.ReplaceAll(nonce, "-", "_")
	draft, err := NewDraftExternalAuditStorage(header.Metadata{},
		ExternalAuditStorageSpec{
			IntegrationName:        integrationName,
			PolicyName:             externalAuditStoragePolicyNamePrefix + nonce,
			Region:                 region,
			SessionRecordingsURI:   externalAuditStorageLongtermBucketPrefix + nonce + "/sessions",
			AuditEventsLongTermURI: externalAuditStorageLongtermBucketPrefix + nonce + "/events",
			AthenaResultsURI:       externalAuditStorageTransientBucketPrefix + nonce + "/query_results",
			AthenaWorkgroup:        "teleport_events_" + underscoreNonce,
			GlueDatabase:           "teleport_events_" + underscoreNonce,
			GlueTable:              "teleport_events",
		})
	return draft, trace.Wrap(err)
}

var (
	// https://docs.aws.amazon.com/AmazonS3/latest/userguide/bucketnamingrules.html
	matchS3BucketName = regexp.MustCompile(`^[a-z0-9\.-]{3,63}$`).MatchString
	// https://docs.aws.amazon.com/AmazonS3/latest/userguide/object-keys.html
	// Taken from the "safe characters" list + /, more restrictive than required.
	matchS3Prefix = regexp.MustCompile(`^[a-zA-Z0-9!_.*'()/-]{0,512}$`).MatchString
)

// ValidateS3URI validates a URI indicating an S3 bucket and prefix for storing
// audit logs (session recordings or events).
func ValidateS3URI(uri string) error {
	// s3:// + at least 3 char bucket name = 8
	if len(uri) < 8 {
		return trace.BadParameter("S3 URI too short")
	}
	// s3:// + 63 char bucket name + / + 1024 char key = 1093
	// ^ this would be the absolute max but we need room after the prefix to
	// store records, so I'll arbitrarily set a limit of 512 here
	if len(uri) > 512 {
		return trace.BadParameter("max length of S3 URI is 512 characters")
	}
	u, err := url.Parse(uri)
	if err != nil {
		return trace.BadParameter("S3 URI failed to parse")
	}
	if !matchS3BucketName(u.Host) {
		return trace.BadParameter("S3 bucket name %q includes illegal special character", u.Host)
	}
	if !matchS3Prefix(u.Path) {
		return trace.BadParameter("S3 prefix %q includes illegal special character", u.Path)
	}
	return nil
}

// NewClusterExternalAuditStorage will create a new cluster External Audit
// Storage.
func NewClusterExternalAuditStorage(metadata header.Metadata, spec ExternalAuditStorageSpec) (*ExternalAuditStorage, error) {
	externalaudit := &ExternalAuditStorage{
		ResourceHeader: header.ResourceHeaderFromMetadata(metadata),
		Spec:           spec,
	}

	name := externalaudit.GetName()
	switch {
	case name == "":
		externalaudit.SetName(types.MetaNameExternalAuditStorageCluster)
	case name != types.MetaNameExternalAuditStorageCluster:
		return nil, trace.BadParameter("cluster External Audit Storage invalid name")
	}

	if err := externalaudit.CheckAndSetDefaults(); err != nil {
		return nil, trace.Wrap(err)
	}
	return externalaudit, nil
}

// CheckAndSetDefaults validates fields and populates empty fields with default values.
func (a *ExternalAuditStorage) CheckAndSetDefaults() error {
	a.SetKind(types.KindExternalAuditStorage)
	a.SetExpiry(time.Time{})
	if version := a.GetVersion(); len(version) == 0 {
		a.SetVersion(types.V1)
	} else if version != types.V1 {
		return trace.BadParameter("unrecognized external_audit_storage version %q", version)
	}

	if err := a.ResourceHeader.CheckAndSetDefaults(); err != nil {
		return trace.Wrap(err)
	}

	if a.Spec.IntegrationName == "" {
		return trace.BadParameter("integration_name required")
	}
	if err := aws.IsValidIAMPolicyName(a.Spec.PolicyName); err != nil {
		return trace.Wrap(err, "validating policy_name")
	}
	if err := aws.IsValidRegion(a.Spec.Region); err != nil {
		return trace.Wrap(err, "validating region")
	}
	if err := ValidateS3URI(a.Spec.SessionRecordingsURI); err != nil {
		return trace.Wrap(err, "validating session_recordings_uri")
	}
	if err := ValidateS3URI(a.Spec.AuditEventsLongTermURI); err != nil {
		return trace.Wrap(err, "validating audit_events_long_term_uri")
	}
	if err := ValidateS3URI(a.Spec.AthenaResultsURI); err != nil {
		return trace.Wrap(err, "validating athena_results_uri")
	}
	if err := aws.IsValidAthenaWorkgroupName(a.Spec.AthenaWorkgroup); err != nil {
		return trace.Wrap(err, "validating athena_workgroup")
	}
	if err := aws.IsValidGlueResourceName(a.Spec.GlueDatabase); err != nil {
		return trace.Wrap(err, "validating glue_database")
	}
	if err := aws.IsValidGlueResourceName(a.Spec.GlueTable); err != nil {
		return trace.Wrap(err, "validating glue_table")
	}

	return nil
}

// GetMetadata returns metadata. This is specifically for conforming to the Resource interface,
// and should be removed when possible.
func (a *ExternalAuditStorage) GetMetadata() types.Metadata {
	return legacy.FromHeaderMetadata(a.Metadata)
}

// MatchSearch goes through select field values of a resource
// and tries to match against the list of search values.
func (a *ExternalAuditStorage) MatchSearch(values []string) bool {
	fieldVals := append(utils.MapToStrings(a.GetAllLabels()), a.GetName())
	return types.MatchSearch(fieldVals, values, nil)
}

// Clone returs a copy of the resource.
func (a *ExternalAuditStorage) Clone() *ExternalAuditStorage {
	var copy *ExternalAuditStorage
	utils.StrictObjectToStruct(a, &copy)
	return copy
}

// CloneResource returns a copy of the resource as types.ResourceWithLabels.
func (a *ExternalAuditStorage) CloneResource() types.ResourceWithLabels {
	return a.Clone()
}
