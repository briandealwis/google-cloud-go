// Copyright 2015 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bigquery

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"cloud.google.com/go/civil"
	datacatalog "cloud.google.com/go/datacatalog/apiv1"
	"cloud.google.com/go/httpreplay"
	"cloud.google.com/go/iam"
	"cloud.google.com/go/internal"
	"cloud.google.com/go/internal/pretty"
	"cloud.google.com/go/internal/testutil"
	"cloud.google.com/go/internal/uid"
	"cloud.google.com/go/storage"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	gax "github.com/googleapis/gax-go/v2"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/iterator"
	"google.golang.org/api/option"
	datacatalogpb "google.golang.org/genproto/googleapis/cloud/datacatalog/v1"
)

const replayFilename = "bigquery.replay"

var record = flag.Bool("record", false, "record RPCs")

var (
	client                 *Client
	storageClient          *storage.Client
	policyTagManagerClient *datacatalog.PolicyTagManagerClient
	dataset                *Dataset
	schema                 = Schema{
		{Name: "name", Type: StringFieldType},
		{Name: "nums", Type: IntegerFieldType, Repeated: true},
		{Name: "rec", Type: RecordFieldType, Schema: Schema{
			{Name: "bool", Type: BooleanFieldType},
		}},
	}
	testTableExpiration                        time.Time
	datasetIDs, tableIDs, modelIDs, routineIDs *uid.Space
)

// Note: integration tests cannot be run in parallel, because TestIntegration_Location
// modifies the client.

func TestMain(m *testing.M) {
	cleanup := initIntegrationTest()
	r := m.Run()
	cleanup()
	os.Exit(r)
}

func getClient(t *testing.T) *Client {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	return client
}

var grpcHeadersChecker = testutil.DefaultHeadersEnforcer()

// If integration tests will be run, create a unique dataset for them.
// Return a cleanup function.
func initIntegrationTest() func() {
	ctx := context.Background()
	flag.Parse() // needed for testing.Short()
	projID := testutil.ProjID()
	switch {
	case testing.Short() && *record:
		log.Fatal("cannot combine -short and -record")
		return func() {}

	case testing.Short() && httpreplay.Supported() && testutil.CanReplay(replayFilename) && projID != "":
		// go test -short with a replay file will replay the integration tests if the
		// environment variables are set.
		log.Printf("replaying from %s", replayFilename)
		httpreplay.DebugHeaders()
		replayer, err := httpreplay.NewReplayer(replayFilename)
		if err != nil {
			log.Fatal(err)
		}
		var t time.Time
		if err := json.Unmarshal(replayer.Initial(), &t); err != nil {
			log.Fatal(err)
		}
		hc, err := replayer.Client(ctx) // no creds needed
		if err != nil {
			log.Fatal(err)
		}
		client, err = NewClient(ctx, projID, option.WithHTTPClient(hc))
		if err != nil {
			log.Fatal(err)
		}
		storageClient, err = storage.NewClient(ctx, option.WithHTTPClient(hc))
		if err != nil {
			log.Fatal(err)
		}
		policyTagManagerClient, err = datacatalog.NewPolicyTagManagerClient(ctx)
		if err != nil {
			log.Fatal(err)
		}
		cleanup := initTestState(client, t)
		return func() {
			cleanup()
			_ = replayer.Close() // No actionable error returned.
		}

	case testing.Short():
		// go test -short without a replay file skips the integration tests.
		if testutil.CanReplay(replayFilename) && projID != "" {
			log.Print("replay not supported for Go versions before 1.8")
		}
		client = nil
		storageClient = nil
		return func() {}

	default: // Run integration tests against a real backend.
		ts := testutil.TokenSource(ctx, Scope)
		if ts == nil {
			log.Println("Integration tests skipped. See CONTRIBUTING.md for details")
			return func() {}
		}
		bqOpts := []option.ClientOption{option.WithTokenSource(ts)}
		sOpts := []option.ClientOption{option.WithTokenSource(testutil.TokenSource(ctx, storage.ScopeFullControl))}
		ptmOpts := []option.ClientOption{option.WithTokenSource(testutil.TokenSource(ctx, "https://www.googleapis.com/auth/cloud-platform"))}
		cleanup := func() {}
		now := time.Now().UTC()
		if *record {
			if !httpreplay.Supported() {
				log.Print("record not supported for Go versions before 1.8")
			} else {
				nowBytes, err := json.Marshal(now)
				if err != nil {
					log.Fatal(err)
				}
				recorder, err := httpreplay.NewRecorder(replayFilename, nowBytes)
				if err != nil {
					log.Fatalf("could not record: %v", err)
				}
				log.Printf("recording to %s", replayFilename)
				hc, err := recorder.Client(ctx, bqOpts...)
				if err != nil {
					log.Fatal(err)
				}
				bqOpts = append(bqOpts, option.WithHTTPClient(hc))
				hc, err = recorder.Client(ctx, sOpts...)
				if err != nil {
					log.Fatal(err)
				}
				sOpts = append(sOpts, option.WithHTTPClient(hc))
				cleanup = func() {
					if err := recorder.Close(); err != nil {
						log.Printf("saving recording: %v", err)
					}
				}
			}
		} else {
			// When we're not recording, do http header checking.
			// We can't check universally because option.WithHTTPClient is
			// incompatible with gRPC options.
			bqOpts = append(bqOpts, grpcHeadersChecker.CallOptions()...)
			sOpts = append(sOpts, grpcHeadersChecker.CallOptions()...)
			ptmOpts = append(ptmOpts, grpcHeadersChecker.CallOptions()...)
		}
		var err error
		client, err = NewClient(ctx, projID, bqOpts...)
		if err != nil {
			log.Fatalf("NewClient: %v", err)
		}
		storageClient, err = storage.NewClient(ctx, sOpts...)
		if err != nil {
			log.Fatalf("storage.NewClient: %v", err)
		}
		policyTagManagerClient, err = datacatalog.NewPolicyTagManagerClient(ctx, ptmOpts...)
		c := initTestState(client, now)
		return func() { c(); cleanup() }
	}
}

func initTestState(client *Client, t time.Time) func() {
	// BigQuery does not accept hyphens in dataset or table IDs, so we create IDs
	// with underscores.
	ctx := context.Background()
	opts := &uid.Options{Sep: '_', Time: t}
	datasetIDs = uid.NewSpace("dataset", opts)
	tableIDs = uid.NewSpace("table", opts)
	modelIDs = uid.NewSpace("model", opts)
	routineIDs = uid.NewSpace("routine", opts)
	testTableExpiration = t.Add(10 * time.Minute).Round(time.Second)
	// For replayability, seed the random source with t.
	Seed(t.UnixNano())

	dataset = client.Dataset(datasetIDs.New())
	if err := dataset.Create(ctx, nil); err != nil {
		log.Fatalf("creating dataset %s: %v", dataset.DatasetID, err)
	}
	return func() {
		if err := dataset.DeleteWithContents(ctx); err != nil {
			log.Printf("could not delete %s", dataset.DatasetID)
		}
	}
}

func TestIntegration_DetectProjectID(t *testing.T) {
	ctx := context.Background()
	testCreds := testutil.Credentials(ctx)
	if testCreds == nil {
		t.Skip("test credentials not present, skipping")
	}

	if _, err := NewClient(ctx, DetectProjectID, option.WithCredentials(testCreds)); err != nil {
		t.Errorf("test NewClient: %v", err)
	}

	badTS := testutil.ErroringTokenSource{}

	if badClient, err := NewClient(ctx, DetectProjectID, option.WithTokenSource(badTS)); err == nil {
		t.Errorf("expected error from bad token source, NewClient succeeded with project: %s", badClient.Project())
	}
}

func TestIntegration_TableCreate(t *testing.T) {
	// Check that creating a record field with an empty schema is an error.
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	table := dataset.Table("t_bad")
	schema := Schema{
		{Name: "rec", Type: RecordFieldType, Schema: Schema{}},
	}
	err := table.Create(context.Background(), &TableMetadata{
		Schema:         schema,
		ExpirationTime: testTableExpiration.Add(5 * time.Minute),
	})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !hasStatusCode(err, http.StatusBadRequest) {
		t.Fatalf("want a 400 error, got %v", err)
	}
}

func TestIntegration_TableCreateWithConstraints(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	table := dataset.Table("constraints")
	schema := Schema{
		{Name: "str_col", Type: StringFieldType, MaxLength: 10},
		{Name: "bytes_col", Type: BytesFieldType, MaxLength: 150},
		{Name: "num_col", Type: NumericFieldType, Precision: 20},
		{Name: "bignumeric_col", Type: BigNumericFieldType, Precision: 30, Scale: 5},
	}
	err := table.Create(context.Background(), &TableMetadata{
		Schema:         schema,
		ExpirationTime: testTableExpiration.Add(5 * time.Minute),
	})
	if err != nil {
		t.Fatalf("table create error: %v", err)
	}

	meta, err := table.Metadata(context.Background())
	if err != nil {
		t.Fatalf("couldn't get metadata: %v", err)
	}

	if diff := testutil.Diff(meta.Schema, schema); diff != "" {
		t.Fatalf("got=-, want=+:\n%s", diff)
	}

}

func TestIntegration_TableCreateView(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	table := newTable(t, schema)
	defer table.Delete(ctx)

	// Test that standard SQL views work.
	view := dataset.Table("t_view_standardsql")
	query := fmt.Sprintf("SELECT APPROX_COUNT_DISTINCT(name) FROM `%s.%s.%s`",
		dataset.ProjectID, dataset.DatasetID, table.TableID)
	err := view.Create(context.Background(), &TableMetadata{
		ViewQuery:      query,
		UseStandardSQL: true,
	})
	if err != nil {
		t.Fatalf("table.create: Did not expect an error, got: %v", err)
	}
	if err := view.Delete(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_TableMetadata(t *testing.T) {

	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	table := newTable(t, schema)
	defer table.Delete(ctx)
	// Check table metadata.
	md, err := table.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// TODO(jba): check md more thorougly.
	if got, want := md.FullID, fmt.Sprintf("%s:%s.%s", dataset.ProjectID, dataset.DatasetID, table.TableID); got != want {
		t.Errorf("metadata.FullID: got %q, want %q", got, want)
	}
	if got, want := md.Type, RegularTable; got != want {
		t.Errorf("metadata.Type: got %v, want %v", got, want)
	}
	if got, want := md.ExpirationTime, testTableExpiration; !got.Equal(want) {
		t.Errorf("metadata.Type: got %v, want %v", got, want)
	}

	// Check that timePartitioning is nil by default
	if md.TimePartitioning != nil {
		t.Errorf("metadata.TimePartitioning: got %v, want %v", md.TimePartitioning, nil)
	}

	// Create tables that have time partitioning
	partitionCases := []struct {
		timePartitioning TimePartitioning
		wantExpiration   time.Duration
		wantField        string
		wantPruneFilter  bool
	}{
		{TimePartitioning{}, time.Duration(0), "", false},
		{TimePartitioning{Expiration: time.Second}, time.Second, "", false},
		{TimePartitioning{RequirePartitionFilter: true}, time.Duration(0), "", true},
		{
			TimePartitioning{
				Expiration:             time.Second,
				Field:                  "date",
				RequirePartitionFilter: true,
			}, time.Second, "date", true},
	}

	schema2 := Schema{
		{Name: "name", Type: StringFieldType},
		{Name: "date", Type: DateFieldType},
	}

	clustering := &Clustering{
		Fields: []string{"name"},
	}

	// Currently, clustering depends on partitioning.  Interleave testing of the two features.
	for i, c := range partitionCases {
		table := dataset.Table(fmt.Sprintf("t_metadata_partition_nocluster_%v", i))
		clusterTable := dataset.Table(fmt.Sprintf("t_metadata_partition_cluster_%v", i))

		// Create unclustered, partitioned variant and get metadata.
		err = table.Create(context.Background(), &TableMetadata{
			Schema:           schema2,
			TimePartitioning: &c.timePartitioning,
			ExpirationTime:   testTableExpiration,
		})
		if err != nil {
			t.Fatal(err)
		}
		defer table.Delete(ctx)
		md, err := table.Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}

		// Created clustered table and get metadata.
		err = clusterTable.Create(context.Background(), &TableMetadata{
			Schema:           schema2,
			TimePartitioning: &c.timePartitioning,
			ExpirationTime:   testTableExpiration,
			Clustering:       clustering,
		})
		if err != nil {
			t.Fatal(err)
		}
		clusterMD, err := clusterTable.Metadata(ctx)
		if err != nil {
			t.Fatal(err)
		}

		for _, v := range []*TableMetadata{md, clusterMD} {
			got := v.TimePartitioning
			want := &TimePartitioning{
				Type:                   DayPartitioningType,
				Expiration:             c.wantExpiration,
				Field:                  c.wantField,
				RequirePartitionFilter: c.wantPruneFilter,
			}
			if !testutil.Equal(got, want) {
				t.Errorf("metadata.TimePartitioning: got %v, want %v", got, want)
			}
			// Manipulate RequirePartitionFilter at the table level.
			mdUpdate := TableMetadataToUpdate{
				RequirePartitionFilter: !want.RequirePartitionFilter,
			}

			newmd, err := table.Update(ctx, mdUpdate, "")
			if err != nil {
				t.Errorf("failed to invert RequirePartitionFilter on %s: %v", table.FullyQualifiedName(), err)
			}
			if newmd.RequirePartitionFilter == want.RequirePartitionFilter {
				t.Errorf("inverting table-level RequirePartitionFilter on %s failed, want %t got %t", table.FullyQualifiedName(), !want.RequirePartitionFilter, newmd.RequirePartitionFilter)
			}
			// Also verify that the clone of RequirePartitionFilter in the TimePartitioning message is consistent.
			if newmd.RequirePartitionFilter != newmd.TimePartitioning.RequirePartitionFilter {
				t.Errorf("inconsistent RequirePartitionFilter.  Table: %t, TimePartitioning: %t", newmd.RequirePartitionFilter, newmd.TimePartitioning.RequirePartitionFilter)
			}

		}

		if md.Clustering != nil {
			t.Errorf("metadata.Clustering was not nil on unclustered table %s", table.TableID)
		}
		got := clusterMD.Clustering
		want := clustering
		if clusterMD.Clustering != clustering {
			if !testutil.Equal(got, want) {
				t.Errorf("metadata.Clustering: got %v, want %v", got, want)
			}
		}
	}

}

func TestIntegration_SnapshotAndRestore(t *testing.T) {

	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()

	// instantiate a base table via a CTAS
	baseTableID := tableIDs.New()
	qualified := fmt.Sprintf("`%s`.%s.%s", testutil.ProjID(), dataset.DatasetID, baseTableID)
	sql := fmt.Sprintf(`
		CREATE TABLE %s
		(
			sample_value INT64,
			groupid STRING,
		)
		AS
		SELECT
		CAST(RAND() * 100 AS INT64),
		CONCAT("group", CAST(CAST(RAND()*10 AS INT64) AS STRING))
		FROM
		UNNEST(GENERATE_ARRAY(0,999))
`, qualified)
	if _, err := runQueryJob(ctx, sql); err != nil {
		t.Fatalf("couldn't instantiate base table: %v", err)
	}

	// Create a snapshot.  We'll select our snapshot time explicitly to validate the snapshot time is the same.
	targetTime := time.Now()
	snapshotID := tableIDs.New()
	copier := dataset.Table(snapshotID).CopierFrom(dataset.Table(fmt.Sprintf("%s@%d", baseTableID, targetTime.UnixNano()/1e6)))
	copier.OperationType = SnapshotOperation
	job, err := copier.Run(ctx)
	if err != nil {
		t.Fatalf("couldn't run snapshot: %v", err)
	}
	status, err := job.Wait(ctx)
	if err != nil {
		t.Fatalf("polling snapshot failed: %v", err)
	}
	if status.Err() != nil {
		t.Fatalf("snapshot failed in error: %v", status.Err())
	}

	// verify metadata on the snapshot
	meta, err := dataset.Table(snapshotID).Metadata(ctx)
	if err != nil {
		t.Fatalf("couldn't get metadata from snapshot: %v", err)
	}
	if meta.Type != Snapshot {
		t.Errorf("expected snapshot table type, got %s", meta.Type)
	}
	want := &SnapshotDefinition{
		BaseTableReference: dataset.Table(baseTableID),
		SnapshotTime:       targetTime,
	}
	if diff := testutil.Diff(meta.SnapshotDefinition, want, cmp.AllowUnexported(Table{}), cmpopts.IgnoreUnexported(Client{}), cmpopts.EquateApproxTime(time.Millisecond)); diff != "" {
		t.Fatalf("SnapshotDefinition differs.  got=-, want=+:\n%s", diff)
	}

	// execute a restore using the snapshot.
	restoreID := tableIDs.New()
	restorer := dataset.Table(restoreID).CopierFrom(dataset.Table(snapshotID))
	restorer.OperationType = RestoreOperation
	job, err = restorer.Run(ctx)
	if err != nil {
		t.Fatalf("couldn't run restore: %v", err)
	}
	status, err = job.Wait(ctx)
	if err != nil {
		t.Fatalf("polling restore failed: %v", err)
	}
	if status.Err() != nil {
		t.Fatalf("restore failed in error: %v", status.Err())
	}

	restoreMeta, err := dataset.Table(restoreID).Metadata(ctx)
	if err != nil {
		t.Fatalf("couldn't get restored table metadata: %v", err)
	}

	if meta.NumBytes != restoreMeta.NumBytes {
		t.Errorf("bytes mismatch.  snap had %d bytes, restore had %d bytes", meta.NumBytes, restoreMeta.NumBytes)
	}
	if meta.NumRows != restoreMeta.NumRows {
		t.Errorf("row counts mismatch.  snap had %d rows, restore had %d rows", meta.NumRows, restoreMeta.NumRows)
	}

}

func TestIntegration_HourTimePartitioning(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	table := dataset.Table(tableIDs.New())

	schema := Schema{
		{Name: "name", Type: StringFieldType},
		{Name: "somevalue", Type: IntegerFieldType},
	}

	// define hourly ingestion-based partitioning.
	wantedTimePartitioning := &TimePartitioning{
		Type: HourPartitioningType,
	}

	err := table.Create(context.Background(), &TableMetadata{
		Schema:           schema,
		TimePartitioning: wantedTimePartitioning,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer table.Delete(ctx)
	md, err := table.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if md.TimePartitioning == nil {
		t.Fatal("expected time partitioning, got nil")
	}
	if diff := testutil.Diff(md.TimePartitioning, wantedTimePartitioning); diff != "" {
		t.Fatalf("got=-, want=+:\n%s", diff)
	}
	if md.TimePartitioning.Type != wantedTimePartitioning.Type {
		t.Errorf("TimePartitioning interval mismatch: got %v, wanted %v", md.TimePartitioning.Type, wantedTimePartitioning.Type)
	}
}

func TestIntegration_RangePartitioning(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	table := dataset.Table(tableIDs.New())

	schema := Schema{
		{Name: "name", Type: StringFieldType},
		{Name: "somevalue", Type: IntegerFieldType},
	}

	wantedRange := &RangePartitioningRange{
		Start:    0,
		End:      135,
		Interval: 25,
	}

	wantedPartitioning := &RangePartitioning{
		Field: "somevalue",
		Range: wantedRange,
	}

	err := table.Create(context.Background(), &TableMetadata{
		Schema:            schema,
		RangePartitioning: wantedPartitioning,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer table.Delete(ctx)
	md, err := table.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if md.RangePartitioning == nil {
		t.Fatal("expected range partitioning, got nil")
	}
	got := md.RangePartitioning.Field
	if wantedPartitioning.Field != got {
		t.Errorf("RangePartitioning Field: got %v, want %v", got, wantedPartitioning.Field)
	}
	if md.RangePartitioning.Range == nil {
		t.Fatal("expected a range definition, got nil")
	}
	gotInt64 := md.RangePartitioning.Range.Start
	if gotInt64 != wantedRange.Start {
		t.Errorf("Range.Start: got %v, wanted %v", gotInt64, wantedRange.Start)
	}
	gotInt64 = md.RangePartitioning.Range.End
	if gotInt64 != wantedRange.End {
		t.Errorf("Range.End: got %v, wanted %v", gotInt64, wantedRange.End)
	}
	gotInt64 = md.RangePartitioning.Range.Interval
	if gotInt64 != wantedRange.Interval {
		t.Errorf("Range.Interval: got %v, wanted %v", gotInt64, wantedRange.Interval)
	}
}
func TestIntegration_RemoveTimePartitioning(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	table := dataset.Table(tableIDs.New())
	want := 24 * time.Hour
	err := table.Create(ctx, &TableMetadata{
		ExpirationTime: testTableExpiration,
		TimePartitioning: &TimePartitioning{
			Expiration: want,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer table.Delete(ctx)

	md, err := table.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := md.TimePartitioning.Expiration; got != want {
		t.Fatalf("TimePartitioning expiration want = %v, got = %v", want, got)
	}

	// Remove time partitioning expiration
	md, err = table.Update(context.Background(), TableMetadataToUpdate{
		TimePartitioning: &TimePartitioning{Expiration: 0},
	}, md.ETag)
	if err != nil {
		t.Fatal(err)
	}

	want = time.Duration(0)
	if got := md.TimePartitioning.Expiration; got != want {
		t.Fatalf("TimeParitioning expiration want = %v, got = %v", want, got)
	}
}

func TestIntegration_DatasetCreate(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	ds := client.Dataset(datasetIDs.New())
	wmd := &DatasetMetadata{Name: "name", Location: "EU"}
	err := ds.Create(ctx, wmd)
	if err != nil {
		t.Fatal(err)
	}
	gmd, err := ds.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := gmd.Name, wmd.Name; got != want {
		t.Errorf("name: got %q, want %q", got, want)
	}
	if got, want := gmd.Location, wmd.Location; got != want {
		t.Errorf("location: got %q, want %q", got, want)
	}
	if err := ds.Delete(ctx); err != nil {
		t.Fatalf("deleting dataset %v: %v", ds, err)
	}
}

func TestIntegration_DatasetMetadata(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	md, err := dataset.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := md.FullID, fmt.Sprintf("%s:%s", dataset.ProjectID, dataset.DatasetID); got != want {
		t.Errorf("FullID: got %q, want %q", got, want)
	}
	jan2016 := time.Date(2016, 1, 1, 0, 0, 0, 0, time.UTC)
	if md.CreationTime.Before(jan2016) {
		t.Errorf("CreationTime: got %s, want > 2016-1-1", md.CreationTime)
	}
	if md.LastModifiedTime.Before(jan2016) {
		t.Errorf("LastModifiedTime: got %s, want > 2016-1-1", md.LastModifiedTime)
	}

	// Verify that we get a NotFound for a nonexistent dataset.
	_, err = client.Dataset("does_not_exist").Metadata(ctx)
	if err == nil || !hasStatusCode(err, http.StatusNotFound) {
		t.Errorf("got %v, want NotFound error", err)
	}
}

func TestIntegration_DatasetDelete(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	ds := client.Dataset(datasetIDs.New())
	if err := ds.Create(ctx, nil); err != nil {
		t.Fatalf("creating dataset %s: %v", ds.DatasetID, err)
	}
	if err := ds.Delete(ctx); err != nil {
		t.Fatalf("deleting dataset %s: %v", ds.DatasetID, err)
	}
}

func TestIntegration_DatasetDeleteWithContents(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	ds := client.Dataset(datasetIDs.New())
	if err := ds.Create(ctx, nil); err != nil {
		t.Fatalf("creating dataset %s: %v", ds.DatasetID, err)
	}
	table := ds.Table(tableIDs.New())
	if err := table.Create(ctx, nil); err != nil {
		t.Fatalf("creating table %s in dataset %s: %v", table.TableID, table.DatasetID, err)
	}
	// We expect failure here
	if err := ds.Delete(ctx); err == nil {
		t.Fatalf("non-recursive delete of dataset %s succeeded unexpectedly.", ds.DatasetID)
	}
	if err := ds.DeleteWithContents(ctx); err != nil {
		t.Fatalf("deleting recursively dataset %s: %v", ds.DatasetID, err)
	}
}

func TestIntegration_DatasetUpdateETags(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}

	check := func(md *DatasetMetadata, wantDesc, wantName string) {
		if md.Description != wantDesc {
			t.Errorf("description: got %q, want %q", md.Description, wantDesc)
		}
		if md.Name != wantName {
			t.Errorf("name: got %q, want %q", md.Name, wantName)
		}
	}

	ctx := context.Background()
	md, err := dataset.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.ETag == "" {
		t.Fatal("empty ETag")
	}
	// Write without ETag succeeds.
	desc := md.Description + "d2"
	name := md.Name + "n2"
	md2, err := dataset.Update(ctx, DatasetMetadataToUpdate{Description: desc, Name: name}, "")
	if err != nil {
		t.Fatal(err)
	}
	check(md2, desc, name)

	// Write with original ETag fails because of intervening write.
	_, err = dataset.Update(ctx, DatasetMetadataToUpdate{Description: "d", Name: "n"}, md.ETag)
	if err == nil {
		t.Fatal("got nil, want error")
	}

	// Write with most recent ETag succeeds.
	md3, err := dataset.Update(ctx, DatasetMetadataToUpdate{Description: "", Name: ""}, md2.ETag)
	if err != nil {
		t.Fatal(err)
	}
	check(md3, "", "")
}

func TestIntegration_DatasetUpdateDefaultExpiration(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	_, err := dataset.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Set the default expiration time.
	md, err := dataset.Update(ctx, DatasetMetadataToUpdate{DefaultTableExpiration: time.Hour}, "")
	if err != nil {
		t.Fatal(err)
	}
	if md.DefaultTableExpiration != time.Hour {
		t.Fatalf("got %s, want 1h", md.DefaultTableExpiration)
	}
	// Omitting DefaultTableExpiration doesn't change it.
	md, err = dataset.Update(ctx, DatasetMetadataToUpdate{Name: "xyz"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if md.DefaultTableExpiration != time.Hour {
		t.Fatalf("got %s, want 1h", md.DefaultTableExpiration)
	}
	// Setting it to 0 deletes it (which looks like a 0 duration).
	md, err = dataset.Update(ctx, DatasetMetadataToUpdate{DefaultTableExpiration: time.Duration(0)}, "")
	if err != nil {
		t.Fatal(err)
	}
	if md.DefaultTableExpiration != 0 {
		t.Fatalf("got %s, want 0", md.DefaultTableExpiration)
	}
}

func TestIntegration_DatasetUpdateAccess(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	md, err := dataset.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Create a sample UDF so we can verify adding authorized UDFs
	routineID := routineIDs.New()
	routine := dataset.Routine(routineID)

	sql := fmt.Sprintf(`
			CREATE FUNCTION `+"`%s`"+`(x INT64) AS (x * 3);`,
		routine.FullyQualifiedName())
	if _, err := runQueryJob(ctx, sql); err != nil {
		t.Fatal(err)
	}
	defer routine.Delete(ctx)

	origAccess := append([]*AccessEntry(nil), md.Access...)
	newEntries := []*AccessEntry{
		{
			Role:       ReaderRole,
			Entity:     "Joe@example.com",
			EntityType: UserEmailEntity,
		},
		{
			Role:       ReaderRole,
			Entity:     "allUsers",
			EntityType: IAMMemberEntity,
		},
		{
			EntityType: RoutineEntity,
			Routine:    routine,
		},
	}

	newAccess := append(md.Access, newEntries...)
	dm := DatasetMetadataToUpdate{Access: newAccess}
	md, err = dataset.Update(ctx, dm, md.ETag)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, err := dataset.Update(ctx, DatasetMetadataToUpdate{Access: origAccess}, md.ETag)
		if err != nil {
			t.Log("could not restore dataset access list")
		}
	}()

	if diff := testutil.Diff(md.Access, newAccess, cmpopts.SortSlices(lessAccessEntries), cmpopts.IgnoreUnexported(Routine{})); diff != "" {
		t.Fatalf("got=-, want=+:\n%s", diff)
	}
}

// Comparison function for AccessEntries to enable order insensitive equality checking.
func lessAccessEntries(x, y *AccessEntry) bool {
	if x.Entity < y.Entity {
		return true
	}
	if x.Entity > y.Entity {
		return false
	}
	if x.EntityType < y.EntityType {
		return true
	}
	if x.EntityType > y.EntityType {
		return false
	}
	if x.Role < y.Role {
		return true
	}
	if x.Role > y.Role {
		return false
	}
	if x.View == nil {
		return y.View != nil
	}
	return false
}

func TestIntegration_DatasetUpdateLabels(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	_, err := dataset.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var dm DatasetMetadataToUpdate
	dm.SetLabel("label", "value")
	md, err := dataset.Update(ctx, dm, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := md.Labels["label"], "value"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	dm = DatasetMetadataToUpdate{}
	dm.DeleteLabel("label")
	md, err = dataset.Update(ctx, dm, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := md.Labels["label"]; ok {
		t.Error("label still present after deletion")
	}
}

func TestIntegration_TableUpdateLabels(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	table := newTable(t, schema)
	defer table.Delete(ctx)

	var tm TableMetadataToUpdate
	tm.SetLabel("label", "value")
	md, err := table.Update(ctx, tm, "")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := md.Labels["label"], "value"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	tm = TableMetadataToUpdate{}
	tm.DeleteLabel("label")
	md, err = table.Update(ctx, tm, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := md.Labels["label"]; ok {
		t.Error("label still present after deletion")
	}
}

func TestIntegration_Tables(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	table := newTable(t, schema)
	defer table.Delete(ctx)
	wantName := table.FullyQualifiedName()

	// This test is flaky due to eventual consistency.
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	err := internal.Retry(ctx, gax.Backoff{}, func() (stop bool, err error) {
		// Iterate over tables in the dataset.
		it := dataset.Tables(ctx)
		var tableNames []string
		for {
			tbl, err := it.Next()
			if err == iterator.Done {
				break
			}
			if err != nil {
				return false, err
			}
			tableNames = append(tableNames, tbl.FullyQualifiedName())
		}
		// Other tests may be running with this dataset, so there might be more
		// than just our table in the list. So don't try for an exact match; just
		// make sure that our table is there somewhere.
		for _, tn := range tableNames {
			if tn == wantName {
				return true, nil
			}
		}
		return false, fmt.Errorf("got %v\nwant %s in the list", tableNames, wantName)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// setupPolicyTag is a helper for setting up policy tags in the datacatalog service.
//
// It returns a string for a policy tag identifier and a cleanup function, or an error.
func setupPolicyTag(ctx context.Context) (string, func(), error) {
	location := "us"
	req := &datacatalogpb.CreateTaxonomyRequest{
		Parent: fmt.Sprintf("projects/%s/locations/%s", testutil.ProjID(), location),
		Taxonomy: &datacatalogpb.Taxonomy{
			DisplayName: "google-cloud-go bigquery testing taxonomy",
			Description: "Taxonomy created for google-cloud-go integration tests",
			ActivatedPolicyTypes: []datacatalogpb.Taxonomy_PolicyType{
				datacatalogpb.Taxonomy_FINE_GRAINED_ACCESS_CONTROL,
			},
		},
	}
	resp, err := policyTagManagerClient.CreateTaxonomy(ctx, req)
	if err != nil {
		return "", nil, fmt.Errorf("datacatalog.CreateTaxonomy: %v", err)
	}
	taxonomyID := resp.GetName()
	cleanupFunc := func() {
		policyTagManagerClient.DeleteTaxonomy(ctx, &datacatalogpb.DeleteTaxonomyRequest{
			Name: taxonomyID,
		})
	}

	tagReq := &datacatalogpb.CreatePolicyTagRequest{
		Parent: resp.GetName(),
		PolicyTag: &datacatalogpb.PolicyTag{
			DisplayName: "ExamplePolicyTag",
		},
	}
	tagResp, err := policyTagManagerClient.CreatePolicyTag(ctx, tagReq)
	if err != nil {
		// we're failed to create tags, but we did create taxonomy. clean it up and signal error.
		cleanupFunc()
		return "", nil, fmt.Errorf("datacatalog.CreatePolicyTag: %v", err)
	}
	return tagResp.GetName(), cleanupFunc, nil
}

func TestIntegration_ColumnACLs(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	testSchema := Schema{
		{Name: "name", Type: StringFieldType},
		{Name: "ssn", Type: StringFieldType},
		{Name: "acct_balance", Type: NumericFieldType},
	}
	table := newTable(t, testSchema)
	defer table.Delete(ctx)

	tagID, cleanupFunc, err := setupPolicyTag(ctx)
	if err != nil {
		t.Fatalf("failed to setup policy tag resources: %v", err)
	}
	defer cleanupFunc()
	// amend the test schema to add a policy tag
	testSchema[1].PolicyTags = &PolicyTagList{
		Names: []string{tagID},
	}

	// Test: Amend an existing schema with a policy tag.
	_, err = table.Update(ctx, TableMetadataToUpdate{
		Schema: testSchema,
	}, "")
	if err != nil {
		t.Errorf("update with policyTag failed: %v", err)
	}

	// Test: Create a new table with a policy tag defined.
	newTable := dataset.Table(tableIDs.New())
	if err = newTable.Create(ctx, &TableMetadata{
		Schema:      schema,
		Description: "foo",
	}); err != nil {
		t.Errorf("failed to create new table with policy tag: %v", err)
	}
}

func TestIntegration_TableIAM(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	table := newTable(t, schema)
	defer table.Delete(ctx)

	// Check to confirm some of our default permissions.
	checkedPerms := []string{"bigquery.tables.get",
		"bigquery.tables.getData", "bigquery.tables.update"}
	perms, err := table.IAM().TestPermissions(ctx, checkedPerms)
	if err != nil {
		t.Fatalf("IAM().TestPermissions: %v", err)
	}
	if len(perms) != len(checkedPerms) {
		t.Errorf("mismatch in permissions, got (%s) wanted (%s)", strings.Join(perms, " "), strings.Join(checkedPerms, " "))
	}

	// Get existing policy, add a binding for all authenticated users.
	policy, err := table.IAM().Policy(ctx)
	if err != nil {
		t.Fatalf("IAM().Policy: %v", err)
	}
	wantedRole := iam.RoleName("roles/bigquery.dataViewer")
	wantedMember := "allAuthenticatedUsers"
	policy.Add(wantedMember, wantedRole)
	if err := table.IAM().SetPolicy(ctx, policy); err != nil {
		t.Fatalf("IAM().SetPolicy: %v", err)
	}

	// Verify policy mutations were persisted by refetching policy.
	updatedPolicy, err := table.IAM().Policy(ctx)
	if err != nil {
		t.Fatalf("IAM.Policy (after update): %v", err)
	}
	foundRole := false
	for _, r := range updatedPolicy.Roles() {
		if r == wantedRole {
			foundRole = true
			break
		}
	}
	if !foundRole {
		t.Errorf("Did not find added role %s in the set of %d roles.",
			wantedRole, len(updatedPolicy.Roles()))
	}
	members := updatedPolicy.Members(wantedRole)
	foundMember := false
	for _, m := range members {
		if m == wantedMember {
			foundMember = true
			break
		}
	}
	if !foundMember {
		t.Errorf("Did not find member %s in role %s", wantedMember, wantedRole)
	}
}

func TestIntegration_SimpleRowResults(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()

	testCases := []struct {
		description string
		query       string
		want        [][]Value
	}{
		{
			description: "literals",
			query:       "select 17 as foo",
			want:        [][]Value{{int64(17)}},
		},
		{
			description: "empty results",
			query:       "SELECT * FROM (select 17 as foo) where false",
			want:        [][]Value{},
		},
		{
			// Previously this would return rows due to the destination reference being present
			// in the job config, but switching to relying on jobs.getQueryResults allows the
			// service to decide the behavior.
			description: "ctas ddl",
			query:       fmt.Sprintf("CREATE TABLE %s.%s AS SELECT 17 as foo", dataset.DatasetID, tableIDs.New()),
			want:        nil,
		},
		{
			// This is a longer running query to ensure probing works as expected.
			description: "long running",
			query:       "select count(*) from unnest(generate_array(1,1000000)), unnest(generate_array(1, 1000)) as foo",
			want:        [][]Value{{int64(1000000000)}},
		},
	}
	for _, tc := range testCases {
		curCase := tc
		t.Run(curCase.description, func(t *testing.T) {
			t.Parallel()
			q := client.Query(curCase.query)
			it, err := q.Read(ctx)
			if err != nil {
				t.Fatalf("%s read error: %v", curCase.description, err)
			}
			checkReadAndTotalRows(t, curCase.description, it, curCase.want)
		})
	}
}

func TestIntegration_QueryIterationPager(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()

	sql := `
	SELECT
		num,
		num * 2 as double
	FROM
		UNNEST(GENERATE_ARRAY(1,5)) as num`
	q := client.Query(sql)
	it, err := q.Read(ctx)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	pager := iterator.NewPager(it, 2, "")
	rowsFetched := 0
	for {
		var rows [][]Value
		nextPageToken, err := pager.NextPage(&rows)
		if err != nil {
			t.Fatalf("NextPage: %v", err)
		}
		rowsFetched = rowsFetched + len(rows)

		if nextPageToken == "" {
			break
		}
	}

	wantRows := 5
	if rowsFetched != wantRows {
		t.Errorf("Expected %d rows, got %d", wantRows, rowsFetched)
	}
}

func TestIntegration_RoutineStoredProcedure(t *testing.T) {
	// Verifies we're exhibiting documented behavior, where we're expected
	// to return the last resultset in a script as the response from a script
	// job.
	// https://github.com/googleapis/google-cloud-go/issues/1974
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()

	// Define a simple stored procedure via DDL.
	routineID := routineIDs.New()
	routine := dataset.Routine(routineID)
	sql := fmt.Sprintf(`
		CREATE OR REPLACE PROCEDURE `+"`%s`"+`(val INT64)
		BEGIN
			SELECT CURRENT_TIMESTAMP() as ts;
			SELECT val * 2 as f2;
		END`,
		routine.FullyQualifiedName())

	if _, err := runQueryJob(ctx, sql); err != nil {
		t.Fatal(err)
	}
	defer routine.Delete(ctx)

	// Invoke the stored procedure.
	sql = fmt.Sprintf(`
	CALL `+"`%s`"+`(5)`,
		routine.FullyQualifiedName())

	q := client.Query(sql)
	it, err := q.Read(ctx)
	if err != nil {
		t.Fatalf("query.Read: %v", err)
	}

	checkReadAndTotalRows(t,
		"expect result set from procedure",
		it, [][]Value{{int64(10)}})
}

func TestIntegration_RoutineUserTVF(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()

	routineID := routineIDs.New()
	routine := dataset.Routine(routineID)
	inMeta := &RoutineMetadata{
		Type:     "TABLE_VALUED_FUNCTION",
		Language: "SQL",
		Arguments: []*RoutineArgument{
			{Name: "filter",
				DataType: &StandardSQLDataType{TypeKind: "INT64"},
			}},
		ReturnTableType: &StandardSQLTableType{
			Columns: []*StandardSQLField{
				{Name: "x", Type: &StandardSQLDataType{TypeKind: "INT64"}},
			},
		},
		Body: "SELECT x FROM UNNEST([1,2,3]) x WHERE x = filter",
	}
	if err := routine.Create(ctx, inMeta); err != nil {
		t.Fatalf("routine create: %v", err)
	}
	defer routine.Delete(ctx)

	meta, err := routine.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Now, compare the input meta to the output meta
	if diff := testutil.Diff(inMeta, meta, cmpopts.IgnoreFields(RoutineMetadata{}, "CreationTime", "LastModifiedTime", "ETag")); diff != "" {
		t.Errorf("routine metadata differs, got=-, want=+\n%s", diff)
	}
}

func TestIntegration_InsertErrors(t *testing.T) {
	// This test serves to verify streaming behavior in the face of oversized data.
	// BigQuery will reject insertAll payloads that exceed a defined limit (10MB).
	// Additionally, if a payload vastly exceeds this limit, the request is rejected
	// by the intermediate architecture.
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	table := newTable(t, schema)
	defer table.Delete(ctx)

	ins := table.Inserter()
	var saverRows []*ValuesSaver

	// badSaver represents an excessively sized (>10MB) row message for insertion.
	badSaver := &ValuesSaver{
		Schema:   schema,
		InsertID: NoDedupeID,
		Row:      []Value{strings.Repeat("X", 10485760), []Value{int64(1)}, []Value{true}},
	}

	saverRows = append(saverRows, badSaver)
	err := ins.Put(ctx, saverRows)
	if err == nil {
		t.Errorf("Wanted row size error, got successful insert.")
	}
	e, ok := err.(*googleapi.Error)
	if !ok {
		t.Errorf("Wanted googleapi.Error, got: %v", err)
	}
	if e.Code != http.StatusRequestEntityTooLarge {
		want := "Request payload size exceeds the limit"
		if !strings.Contains(e.Message, want) {
			t.Errorf("Error didn't contain expected message (%s): %#v", want, e)
		}
	}
	// Case 2: Very Large Request
	// Request so large it gets rejected by intermediate infra (3x 10MB rows)
	saverRows = append(saverRows, badSaver)
	saverRows = append(saverRows, badSaver)

	err = ins.Put(ctx, saverRows)
	if err == nil {
		t.Errorf("Wanted error, got successful insert.")
	}
	e, ok = err.(*googleapi.Error)
	if !ok {
		t.Errorf("wanted googleapi.Error, got: %v", err)
	}
	if e.Code != http.StatusBadRequest && e.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("Wanted HTTP 400 or 413, got %d", e.Code)
	}
}

func TestIntegration_InsertAndRead(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	table := newTable(t, schema)
	defer table.Delete(ctx)

	// Populate the table.
	ins := table.Inserter()
	var (
		wantRows  [][]Value
		saverRows []*ValuesSaver
	)
	for i, name := range []string{"a", "b", "c"} {
		row := []Value{name, []Value{int64(i)}, []Value{true}}
		wantRows = append(wantRows, row)
		saverRows = append(saverRows, &ValuesSaver{
			Schema:   schema,
			InsertID: name,
			Row:      row,
		})
	}
	if err := ins.Put(ctx, saverRows); err != nil {
		t.Fatal(putError(err))
	}

	// Wait until the data has been uploaded. This can take a few seconds, according
	// to https://cloud.google.com/bigquery/streaming-data-into-bigquery.
	if err := waitForRow(ctx, table); err != nil {
		t.Fatal(err)
	}
	// Read the table.
	checkRead(t, "upload", table.Read(ctx), wantRows)

	// Query the table.
	q := client.Query(fmt.Sprintf("select name, nums, rec from %s", table.TableID))
	q.DefaultProjectID = dataset.ProjectID
	q.DefaultDatasetID = dataset.DatasetID

	rit, err := q.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checkRead(t, "query", rit, wantRows)

	// Query the long way.
	job1, err := q.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if job1.LastStatus() == nil {
		t.Error("no LastStatus")
	}
	job2, err := client.JobFromID(ctx, job1.ID())
	if err != nil {
		t.Fatal(err)
	}
	if job2.LastStatus() == nil {
		t.Error("no LastStatus")
	}
	rit, err = job2.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checkRead(t, "job.Read", rit, wantRows)

	// Get statistics.
	jobStatus, err := job2.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if jobStatus.Statistics == nil {
		t.Fatal("jobStatus missing statistics")
	}
	if _, ok := jobStatus.Statistics.Details.(*QueryStatistics); !ok {
		t.Errorf("expected QueryStatistics, got %T", jobStatus.Statistics.Details)
	}

	// Test reading directly into a []Value.
	valueLists, schema, _, err := readAll(table.Read(ctx))
	if err != nil {
		t.Fatal(err)
	}
	it := table.Read(ctx)
	for i, vl := range valueLists {
		var got []Value
		if err := it.Next(&got); err != nil {
			t.Fatal(err)
		}
		if !testutil.Equal(it.Schema, schema) {
			t.Fatalf("got schema %v, want %v", it.Schema, schema)
		}
		want := []Value(vl)
		if !testutil.Equal(got, want) {
			t.Errorf("%d: got %v, want %v", i, got, want)
		}
	}

	// Test reading into a map.
	it = table.Read(ctx)
	for _, vl := range valueLists {
		var vm map[string]Value
		if err := it.Next(&vm); err != nil {
			t.Fatal(err)
		}
		if got, want := len(vm), len(vl); got != want {
			t.Fatalf("valueMap len: got %d, want %d", got, want)
		}
		// With maps, structs become nested maps.
		vl[2] = map[string]Value{"bool": vl[2].([]Value)[0]}
		for i, v := range vl {
			if got, want := vm[schema[i].Name], v; !testutil.Equal(got, want) {
				t.Errorf("%d, name=%s: got %#v, want %#v",
					i, schema[i].Name, got, want)
			}
		}
	}

}

type SubSubTestStruct struct {
	Integer int64
}

type SubTestStruct struct {
	String      string
	Record      SubSubTestStruct
	RecordArray []SubSubTestStruct
}

type TestStruct struct {
	Name      string
	Bytes     []byte
	Integer   int64
	Float     float64
	Boolean   bool
	Timestamp time.Time
	Date      civil.Date
	Time      civil.Time
	DateTime  civil.DateTime
	Numeric   *big.Rat
	Geography string

	StringArray    []string
	IntegerArray   []int64
	FloatArray     []float64
	BooleanArray   []bool
	TimestampArray []time.Time
	DateArray      []civil.Date
	TimeArray      []civil.Time
	DateTimeArray  []civil.DateTime
	NumericArray   []*big.Rat
	GeographyArray []string

	Record      SubTestStruct
	RecordArray []SubTestStruct
}

// Round times to the microsecond for comparison purposes.
var roundToMicros = cmp.Transformer("RoundToMicros",
	func(t time.Time) time.Time { return t.Round(time.Microsecond) })

func TestIntegration_InsertAndReadStructs(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	schema, err := InferSchema(TestStruct{})
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	table := newTable(t, schema)
	defer table.Delete(ctx)

	d := civil.Date{Year: 2016, Month: 3, Day: 20}
	tm := civil.Time{Hour: 15, Minute: 4, Second: 5, Nanosecond: 6000}
	ts := time.Date(2016, 3, 20, 15, 4, 5, 6000, time.UTC)
	dtm := civil.DateTime{Date: d, Time: tm}
	d2 := civil.Date{Year: 1994, Month: 5, Day: 15}
	tm2 := civil.Time{Hour: 1, Minute: 2, Second: 4, Nanosecond: 0}
	ts2 := time.Date(1994, 5, 15, 1, 2, 4, 0, time.UTC)
	dtm2 := civil.DateTime{Date: d2, Time: tm2}
	g := "POINT(-122.350220 47.649154)"
	g2 := "POINT(-122.0836791 37.421827)"

	// Populate the table.
	ins := table.Inserter()
	want := []*TestStruct{
		{
			"a",
			[]byte("byte"),
			42,
			3.14,
			true,
			ts,
			d,
			tm,
			dtm,
			big.NewRat(57, 100),
			g,
			[]string{"a", "b"},
			[]int64{1, 2},
			[]float64{1, 1.41},
			[]bool{true, false},
			[]time.Time{ts, ts2},
			[]civil.Date{d, d2},
			[]civil.Time{tm, tm2},
			[]civil.DateTime{dtm, dtm2},
			[]*big.Rat{big.NewRat(1, 2), big.NewRat(3, 5)},
			[]string{g, g2},
			SubTestStruct{
				"string",
				SubSubTestStruct{24},
				[]SubSubTestStruct{{1}, {2}},
			},
			[]SubTestStruct{
				{String: "empty"},
				{
					"full",
					SubSubTestStruct{1},
					[]SubSubTestStruct{{1}, {2}},
				},
			},
		},
		{
			Name:      "b",
			Bytes:     []byte("byte2"),
			Integer:   24,
			Float:     4.13,
			Boolean:   false,
			Timestamp: ts,
			Date:      d,
			Time:      tm,
			DateTime:  dtm,
			Numeric:   big.NewRat(4499, 10000),
		},
	}
	var savers []*StructSaver
	for _, s := range want {
		savers = append(savers, &StructSaver{Schema: schema, Struct: s})
	}
	if err := ins.Put(ctx, savers); err != nil {
		t.Fatal(putError(err))
	}

	// Wait until the data has been uploaded. This can take a few seconds, according
	// to https://cloud.google.com/bigquery/streaming-data-into-bigquery.
	if err := waitForRow(ctx, table); err != nil {
		t.Fatal(err)
	}

	// Test iteration with structs.
	it := table.Read(ctx)
	var got []*TestStruct
	for {
		var g TestStruct
		err := it.Next(&g)
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, &g)
	}
	sort.Sort(byName(got))

	// BigQuery does not elide nils. It reports an error for nil fields.
	for i, g := range got {
		if i >= len(want) {
			t.Errorf("%d: got %v, past end of want", i, pretty.Value(g))
		} else if diff := testutil.Diff(g, want[i], roundToMicros); diff != "" {
			t.Errorf("%d: got=-, want=+:\n%s", i, diff)
		}
	}
}

type byName []*TestStruct

func (b byName) Len() int           { return len(b) }
func (b byName) Swap(i, j int)      { b[i], b[j] = b[j], b[i] }
func (b byName) Less(i, j int) bool { return b[i].Name < b[j].Name }

func TestIntegration_InsertAndReadNullable(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctm := civil.Time{Hour: 15, Minute: 4, Second: 5, Nanosecond: 6000}
	cdt := civil.DateTime{Date: testDate, Time: ctm}
	rat := big.NewRat(33, 100)
	rat2 := big.NewRat(66, 100)
	geo := "POINT(-122.198939 47.669865)"

	// Nil fields in the struct.
	testInsertAndReadNullable(t, testStructNullable{}, make([]Value, len(testStructNullableSchema)))

	// Explicitly invalidate the Null* types within the struct.
	testInsertAndReadNullable(t, testStructNullable{
		String:    NullString{Valid: false},
		Integer:   NullInt64{Valid: false},
		Float:     NullFloat64{Valid: false},
		Boolean:   NullBool{Valid: false},
		Timestamp: NullTimestamp{Valid: false},
		Date:      NullDate{Valid: false},
		Time:      NullTime{Valid: false},
		DateTime:  NullDateTime{Valid: false},
		Geography: NullGeography{Valid: false},
	},
		make([]Value, len(testStructNullableSchema)))

	// Populate the struct with values.
	testInsertAndReadNullable(t, testStructNullable{
		String:     NullString{"x", true},
		Bytes:      []byte{1, 2, 3},
		Integer:    NullInt64{1, true},
		Float:      NullFloat64{2.3, true},
		Boolean:    NullBool{true, true},
		Timestamp:  NullTimestamp{testTimestamp, true},
		Date:       NullDate{testDate, true},
		Time:       NullTime{ctm, true},
		DateTime:   NullDateTime{cdt, true},
		Numeric:    rat,
		BigNumeric: rat2,
		Geography:  NullGeography{geo, true},
		Record:     &subNullable{X: NullInt64{4, true}},
	},
		[]Value{"x", []byte{1, 2, 3}, int64(1), 2.3, true, testTimestamp, testDate, ctm, cdt, rat, rat2, geo, []Value{int64(4)}})
}

func testInsertAndReadNullable(t *testing.T, ts testStructNullable, wantRow []Value) {
	ctx := context.Background()
	table := newTable(t, testStructNullableSchema)
	defer table.Delete(ctx)

	// Populate the table.
	ins := table.Inserter()
	if err := ins.Put(ctx, []*StructSaver{{Schema: testStructNullableSchema, Struct: ts}}); err != nil {
		t.Fatal(putError(err))
	}
	// Wait until the data has been uploaded. This can take a few seconds, according
	// to https://cloud.google.com/bigquery/streaming-data-into-bigquery.
	if err := waitForRow(ctx, table); err != nil {
		t.Fatal(err)
	}

	// Read into a []Value.
	iter := table.Read(ctx)
	gotRows, _, _, err := readAll(iter)
	if err != nil {
		t.Fatal(err)
	}
	if len(gotRows) != 1 {
		t.Fatalf("got %d rows, want 1", len(gotRows))
	}
	if diff := testutil.Diff(gotRows[0], wantRow, roundToMicros); diff != "" {
		t.Error(diff)
	}

	// Read into a struct.
	want := ts
	var sn testStructNullable
	it := table.Read(ctx)
	if err := it.Next(&sn); err != nil {
		t.Fatal(err)
	}
	if diff := testutil.Diff(sn, want, roundToMicros); diff != "" {
		t.Error(diff)
	}
}

func TestIntegration_TableUpdate(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	table := newTable(t, schema)
	defer table.Delete(ctx)

	// Test Update of non-schema fields.
	tm, err := table.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantDescription := tm.Description + "more"
	wantName := tm.Name + "more"
	wantExpiration := tm.ExpirationTime.Add(time.Hour * 24)
	got, err := table.Update(ctx, TableMetadataToUpdate{
		Description:    wantDescription,
		Name:           wantName,
		ExpirationTime: wantExpiration,
	}, tm.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if got.Description != wantDescription {
		t.Errorf("Description: got %q, want %q", got.Description, wantDescription)
	}
	if got.Name != wantName {
		t.Errorf("Name: got %q, want %q", got.Name, wantName)
	}
	if got.ExpirationTime != wantExpiration {
		t.Errorf("ExpirationTime: got %q, want %q", got.ExpirationTime, wantExpiration)
	}
	if !testutil.Equal(got.Schema, schema) {
		t.Errorf("Schema: got %v, want %v", pretty.Value(got.Schema), pretty.Value(schema))
	}

	// Blind write succeeds.
	_, err = table.Update(ctx, TableMetadataToUpdate{Name: "x"}, "")
	if err != nil {
		t.Fatal(err)
	}
	// Write with old etag fails.
	_, err = table.Update(ctx, TableMetadataToUpdate{Name: "y"}, got.ETag)
	if err == nil {
		t.Fatal("Update with old ETag succeeded, wanted failure")
	}

	// Test schema update.
	// Columns can be added. schema2 is the same as schema, except for the
	// added column in the middle.
	nested := Schema{
		{Name: "nested", Type: BooleanFieldType},
		{Name: "other", Type: StringFieldType},
	}
	schema2 := Schema{
		schema[0],
		{Name: "rec2", Type: RecordFieldType, Schema: nested},
		schema[1],
		schema[2],
	}

	got, err = table.Update(ctx, TableMetadataToUpdate{Schema: schema2}, "")
	if err != nil {
		t.Fatal(err)
	}

	// Wherever you add the column, it appears at the end.
	schema3 := Schema{schema2[0], schema2[2], schema2[3], schema2[1]}
	if !testutil.Equal(got.Schema, schema3) {
		t.Errorf("add field:\ngot  %v\nwant %v",
			pretty.Value(got.Schema), pretty.Value(schema3))
	}

	// Updating with the empty schema succeeds, but is a no-op.
	got, err = table.Update(ctx, TableMetadataToUpdate{Schema: Schema{}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !testutil.Equal(got.Schema, schema3) {
		t.Errorf("empty schema:\ngot  %v\nwant %v",
			pretty.Value(got.Schema), pretty.Value(schema3))
	}

	// Error cases when updating schema.
	for _, test := range []struct {
		desc   string
		fields Schema
	}{
		{"change from optional to required", Schema{
			{Name: "name", Type: StringFieldType, Required: true},
			schema3[1],
			schema3[2],
			schema3[3],
		}},
		{"add a required field", Schema{
			schema3[0], schema3[1], schema3[2], schema3[3],
			{Name: "req", Type: StringFieldType, Required: true},
		}},
		{"remove a field", Schema{schema3[0], schema3[1], schema3[2]}},
		{"remove a nested field", Schema{
			schema3[0], schema3[1], schema3[2],
			{Name: "rec2", Type: RecordFieldType, Schema: Schema{nested[0]}}}},
		{"remove all nested fields", Schema{
			schema3[0], schema3[1], schema3[2],
			{Name: "rec2", Type: RecordFieldType, Schema: Schema{}}}},
	} {
		_, err = table.Update(ctx, TableMetadataToUpdate{Schema: Schema(test.fields)}, "")
		if err == nil {
			t.Errorf("%s: want error, got nil", test.desc)
		} else if !hasStatusCode(err, 400) {
			t.Errorf("%s: want 400, got %v", test.desc, err)
		}
	}
}

func TestIntegration_QueryStatistics(t *testing.T) {
	// Make a bunch of assertions on a simple query.
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()

	q := client.Query("SELECT 17 as foo, 3.14 as bar")
	// disable cache to ensure we have query statistics
	q.DisableQueryCache = true

	job, err := q.Run(ctx)
	if err != nil {
		t.Fatalf("job Run failure: %v", err)
	}
	status, err := job.Wait(ctx)
	if err != nil {
		t.Fatalf("job Wait failure: %v", err)
	}
	if status.Statistics == nil {
		t.Fatal("expected job statistics, none found")
	}

	if status.Statistics.NumChildJobs != 0 {
		t.Errorf("expected no children, %d reported", status.Statistics.NumChildJobs)
	}

	if status.Statistics.ParentJobID != "" {
		t.Errorf("expected no parent, but parent present: %s", status.Statistics.ParentJobID)
	}

	if status.Statistics.Details == nil {
		t.Fatal("expected job details, none present")
	}

	qStats, ok := status.Statistics.Details.(*QueryStatistics)
	if !ok {
		t.Fatalf("expected query statistics not present")
	}

	if qStats.CacheHit {
		t.Error("unexpected cache hit")
	}

	if qStats.StatementType != "SELECT" {
		t.Errorf("expected SELECT statement type, got: %s", qStats.StatementType)
	}

	if len(qStats.QueryPlan) == 0 {
		t.Error("expected query plan, none present")
	}

	if len(qStats.Timeline) == 0 {
		t.Error("expected query timeline, none present")
	}
}

func TestIntegration_Load(t *testing.T) {
	t.Skip("https://github.com/googleapis/google-cloud-go/issues/4418")
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	// CSV data can't be loaded into a repeated field, so we use a different schema.
	table := newTable(t, Schema{
		{Name: "name", Type: StringFieldType},
		{Name: "nums", Type: IntegerFieldType},
	})
	defer table.Delete(ctx)

	// Load the table from a reader.
	r := strings.NewReader("a,0\nb,1\nc,2\n")
	wantRows := [][]Value{
		{"a", int64(0)},
		{"b", int64(1)},
		{"c", int64(2)},
	}
	rs := NewReaderSource(r)
	loader := table.LoaderFrom(rs)
	loader.WriteDisposition = WriteTruncate
	loader.Labels = map[string]string{"test": "go"}
	job, err := loader.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if job.LastStatus() == nil {
		t.Error("no LastStatus")
	}
	conf, err := job.Config()
	if err != nil {
		t.Fatal(err)
	}
	config, ok := conf.(*LoadConfig)
	if !ok {
		t.Fatalf("got %T, want LoadConfig", conf)
	}
	diff := testutil.Diff(config, &loader.LoadConfig,
		cmp.AllowUnexported(Table{}),
		cmpopts.IgnoreUnexported(Client{}, ReaderSource{}),
		// returned schema is at top level, not in the config
		cmpopts.IgnoreFields(FileConfig{}, "Schema"))
	if diff != "" {
		t.Errorf("got=-, want=+:\n%s", diff)
	}
	if err := wait(ctx, job); err != nil {
		t.Fatal(err)
	}
	checkReadAndTotalRows(t, "reader load", table.Read(ctx), wantRows)

}

func TestIntegration_DML(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	table := newTable(t, schema)
	defer table.Delete(ctx)

	sql := fmt.Sprintf(`INSERT %s.%s (name, nums, rec)
						VALUES ('a', [0], STRUCT<BOOL>(TRUE)),
							   ('b', [1], STRUCT<BOOL>(FALSE)),
							   ('c', [2], STRUCT<BOOL>(TRUE))`,
		table.DatasetID, table.TableID)
	stats, err := runQueryJob(ctx, sql)
	if err != nil {
		t.Fatal(err)
	}
	wantRows := [][]Value{
		{"a", []Value{int64(0)}, []Value{true}},
		{"b", []Value{int64(1)}, []Value{false}},
		{"c", []Value{int64(2)}, []Value{true}},
	}
	checkRead(t, "DML", table.Read(ctx), wantRows)
	if stats == nil {
		t.Fatalf("no query stats")
	}
	if stats.DMLStats == nil {
		t.Fatalf("no dml stats")
	}
	wantRowCount := int64(len(wantRows))
	if stats.DMLStats.InsertedRowCount != wantRowCount {
		t.Fatalf("dml stats mismatch.  got %d inserted rows, want %d", stats.DMLStats.InsertedRowCount, wantRowCount)
	}
}

// runQueryJob is useful for running queries where no row data is returned (DDL/DML).
func runQueryJob(ctx context.Context, sql string) (*QueryStatistics, error) {
	var stats *QueryStatistics
	var err error
	err = internal.Retry(ctx, gax.Backoff{}, func() (stop bool, err error) {
		job, err := client.Query(sql).Run(ctx)
		if err != nil {
			if e, ok := err.(*googleapi.Error); ok && e.Code < 500 {
				return true, err // fail on 4xx
			}
			return false, err
		}
		_, err = job.Wait(ctx)
		if err != nil {
			if e, ok := err.(*googleapi.Error); ok && e.Code < 500 {
				return true, err // fail on 4xx
			}
			return false, err
		}
		status := job.LastStatus()
		if status.Statistics != nil {
			if qStats, ok := status.Statistics.Details.(*QueryStatistics); ok {
				stats = qStats
			}
		}
		return true, nil
	})
	return stats, err
}

func TestIntegration_TimeTypes(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	dtSchema := Schema{
		{Name: "d", Type: DateFieldType},
		{Name: "t", Type: TimeFieldType},
		{Name: "dt", Type: DateTimeFieldType},
		{Name: "ts", Type: TimestampFieldType},
	}
	table := newTable(t, dtSchema)
	defer table.Delete(ctx)

	d := civil.Date{Year: 2016, Month: 3, Day: 20}
	tm := civil.Time{Hour: 12, Minute: 30, Second: 0, Nanosecond: 6000}
	dtm := civil.DateTime{Date: d, Time: tm}
	ts := time.Date(2016, 3, 20, 15, 04, 05, 0, time.UTC)
	wantRows := [][]Value{
		{d, tm, dtm, ts},
	}
	ins := table.Inserter()
	if err := ins.Put(ctx, []*ValuesSaver{
		{Schema: dtSchema, Row: wantRows[0]},
	}); err != nil {
		t.Fatal(putError(err))
	}
	if err := waitForRow(ctx, table); err != nil {
		t.Fatal(err)
	}

	// SQL wants DATETIMEs with a space between date and time, but the service
	// returns them in RFC3339 form, with a "T" between.
	query := fmt.Sprintf("INSERT %s.%s (d, t, dt, ts) "+
		"VALUES ('%s', '%s', '%s', '%s')",
		table.DatasetID, table.TableID,
		d, CivilTimeString(tm), CivilDateTimeString(dtm), ts.Format("2006-01-02 15:04:05"))
	if _, err := runQueryJob(ctx, query); err != nil {
		t.Fatal(err)
	}
	wantRows = append(wantRows, wantRows[0])
	checkRead(t, "TimeTypes", table.Read(ctx), wantRows)
}

func TestIntegration_StandardQuery(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()

	d := civil.Date{Year: 2016, Month: 3, Day: 20}
	tm := civil.Time{Hour: 15, Minute: 04, Second: 05, Nanosecond: 0}
	ts := time.Date(2016, 3, 20, 15, 04, 05, 0, time.UTC)
	dtm := ts.Format("2006-01-02 15:04:05")

	// Constructs Value slices made up of int64s.
	ints := func(args ...int) []Value {
		vals := make([]Value, len(args))
		for i, arg := range args {
			vals[i] = int64(arg)
		}
		return vals
	}

	testCases := []struct {
		query   string
		wantRow []Value
	}{
		{"SELECT 1", ints(1)},
		{"SELECT 1.3", []Value{1.3}},
		{"SELECT CAST(1.3  AS NUMERIC)", []Value{big.NewRat(13, 10)}},
		{"SELECT NUMERIC '0.25'", []Value{big.NewRat(1, 4)}},
		{"SELECT TRUE", []Value{true}},
		{"SELECT 'ABC'", []Value{"ABC"}},
		{"SELECT CAST('foo' AS BYTES)", []Value{[]byte("foo")}},
		{fmt.Sprintf("SELECT TIMESTAMP '%s'", dtm), []Value{ts}},
		{fmt.Sprintf("SELECT [TIMESTAMP '%s', TIMESTAMP '%s']", dtm, dtm), []Value{[]Value{ts, ts}}},
		{fmt.Sprintf("SELECT ('hello', TIMESTAMP '%s')", dtm), []Value{[]Value{"hello", ts}}},
		{fmt.Sprintf("SELECT DATETIME(TIMESTAMP '%s')", dtm), []Value{civil.DateTime{Date: d, Time: tm}}},
		{fmt.Sprintf("SELECT DATE(TIMESTAMP '%s')", dtm), []Value{d}},
		{fmt.Sprintf("SELECT TIME(TIMESTAMP '%s')", dtm), []Value{tm}},
		{"SELECT (1, 2)", []Value{ints(1, 2)}},
		{"SELECT [1, 2, 3]", []Value{ints(1, 2, 3)}},
		{"SELECT ([1, 2], 3, [4, 5])", []Value{[]Value{ints(1, 2), int64(3), ints(4, 5)}}},
		{"SELECT [(1, 2, 3), (4, 5, 6)]", []Value{[]Value{ints(1, 2, 3), ints(4, 5, 6)}}},
		{"SELECT [([1, 2, 3], 4), ([5, 6], 7)]", []Value{[]Value{[]Value{ints(1, 2, 3), int64(4)}, []Value{ints(5, 6), int64(7)}}}},
		{"SELECT ARRAY(SELECT STRUCT([1, 2]))", []Value{[]Value{[]Value{ints(1, 2)}}}},
	}
	for _, c := range testCases {
		q := client.Query(c.query)
		it, err := q.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		checkRead(t, "StandardQuery", it, [][]Value{c.wantRow})
	}
}

func TestIntegration_LegacyQuery(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()

	ts := time.Date(2016, 3, 20, 15, 04, 05, 0, time.UTC)
	dtm := ts.Format("2006-01-02 15:04:05")

	testCases := []struct {
		query   string
		wantRow []Value
	}{
		{"SELECT 1", []Value{int64(1)}},
		{"SELECT 1.3", []Value{1.3}},
		{"SELECT TRUE", []Value{true}},
		{"SELECT 'ABC'", []Value{"ABC"}},
		{"SELECT CAST('foo' AS BYTES)", []Value{[]byte("foo")}},
		{fmt.Sprintf("SELECT TIMESTAMP('%s')", dtm), []Value{ts}},
		{fmt.Sprintf("SELECT DATE(TIMESTAMP('%s'))", dtm), []Value{"2016-03-20"}},
		{fmt.Sprintf("SELECT TIME(TIMESTAMP('%s'))", dtm), []Value{"15:04:05"}},
	}
	for _, c := range testCases {
		q := client.Query(c.query)
		q.UseLegacySQL = true
		it, err := q.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		checkRead(t, "LegacyQuery", it, [][]Value{c.wantRow})
	}
}

func TestIntegration_IteratorSource(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	q := client.Query("SELECT 17 as foo")
	it, err := q.Read(ctx)
	if err != nil {
		t.Errorf("Read: %v", err)
	}
	src := it.SourceJob()
	if src == nil {
		t.Errorf("wanted source job, got nil")
	}
	status, err := src.Status(ctx)
	if err != nil {
		t.Errorf("Status: %v", err)
	}
	if status == nil {
		t.Errorf("got nil status")
	}
}

func TestIntegration_QueryExternalHivePartitioning(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()

	autoTable := dataset.Table(tableIDs.New())
	customTable := dataset.Table(tableIDs.New())

	err := autoTable.Create(ctx, &TableMetadata{
		ExternalDataConfig: &ExternalDataConfig{
			SourceFormat:       Parquet,
			SourceURIs:         []string{"gs://cloud-samples-data/bigquery/hive-partitioning-samples/autolayout/*"},
			AutoDetect:         true,
			DecimalTargetTypes: []DecimalTargetType{StringTargetType},
			HivePartitioningOptions: &HivePartitioningOptions{
				Mode:                   AutoHivePartitioningMode,
				SourceURIPrefix:        "gs://cloud-samples-data/bigquery/hive-partitioning-samples/autolayout/",
				RequirePartitionFilter: true,
			},
		},
	})
	if err != nil {
		t.Fatalf("table.Create(auto): %v", err)
	}
	defer autoTable.Delete(ctx)

	err = customTable.Create(ctx, &TableMetadata{
		ExternalDataConfig: &ExternalDataConfig{
			SourceFormat:       Parquet,
			SourceURIs:         []string{"gs://cloud-samples-data/bigquery/hive-partitioning-samples/customlayout/*"},
			AutoDetect:         true,
			DecimalTargetTypes: []DecimalTargetType{NumericTargetType, StringTargetType},
			HivePartitioningOptions: &HivePartitioningOptions{
				Mode:                   CustomHivePartitioningMode,
				SourceURIPrefix:        "gs://cloud-samples-data/bigquery/hive-partitioning-samples/customlayout/{pkey:STRING}/",
				RequirePartitionFilter: true,
			},
		},
	})
	if err != nil {
		t.Fatalf("table.Create(custom): %v", err)
	}
	defer customTable.Delete(ctx)

	// Issue a test query that prunes based on the custom hive partitioning key, and verify the result is as expected.
	sql := fmt.Sprintf("SELECT COUNT(*) as ct FROM `%s`.%s.%s WHERE pkey=\"foo\"", customTable.ProjectID, customTable.DatasetID, customTable.TableID)
	q := client.Query(sql)
	it, err := q.Read(ctx)
	if err != nil {
		t.Fatalf("Error querying: %v", err)
	}
	checkReadAndTotalRows(t, "HiveQuery", it, [][]Value{{int64(50)}})
}

func TestIntegration_QueryParameters(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()

	d := civil.Date{Year: 2016, Month: 3, Day: 20}
	tm := civil.Time{Hour: 15, Minute: 04, Second: 05, Nanosecond: 3008}
	rtm := tm
	rtm.Nanosecond = 3000 // round to microseconds
	dtm := civil.DateTime{Date: d, Time: tm}
	ts := time.Date(2016, 3, 20, 15, 04, 05, 0, time.UTC)
	rat := big.NewRat(13, 10)

	type ss struct {
		String string
	}

	type s struct {
		Timestamp      time.Time
		StringArray    []string
		SubStruct      ss
		SubStructArray []ss
	}

	testCases := []struct {
		query      string
		parameters []QueryParameter
		wantRow    []Value
		wantConfig interface{}
	}{
		{
			"SELECT @val",
			[]QueryParameter{{"val", 1}},
			[]Value{int64(1)},
			int64(1),
		},
		{
			"SELECT @val",
			[]QueryParameter{{"val", 1.3}},
			[]Value{1.3},
			1.3,
		},
		{
			"SELECT @val",
			[]QueryParameter{{"val", rat}},
			[]Value{rat},
			rat,
		},
		{
			"SELECT @val",
			[]QueryParameter{{"val", true}},
			[]Value{true},
			true,
		},
		{
			"SELECT @val",
			[]QueryParameter{{"val", "ABC"}},
			[]Value{"ABC"},
			"ABC",
		},
		{
			"SELECT @val",
			[]QueryParameter{{"val", []byte("foo")}},
			[]Value{[]byte("foo")},
			[]byte("foo"),
		},
		{
			"SELECT @val",
			[]QueryParameter{{"val", ts}},
			[]Value{ts},
			ts,
		},
		{
			"SELECT @val",
			[]QueryParameter{{"val", []time.Time{ts, ts}}},
			[]Value{[]Value{ts, ts}},
			[]interface{}{ts, ts},
		},
		{
			"SELECT @val",
			[]QueryParameter{{"val", dtm}},
			[]Value{civil.DateTime{Date: d, Time: rtm}},
			civil.DateTime{Date: d, Time: rtm},
		},
		{
			"SELECT @val",
			[]QueryParameter{{"val", d}},
			[]Value{d},
			d,
		},
		{
			"SELECT @val",
			[]QueryParameter{{"val", tm}},
			[]Value{rtm},
			rtm,
		},
		{
			"SELECT @val",
			[]QueryParameter{{"val", s{ts, []string{"a", "b"}, ss{"c"}, []ss{{"d"}, {"e"}}}}},
			[]Value{[]Value{ts, []Value{"a", "b"}, []Value{"c"}, []Value{[]Value{"d"}, []Value{"e"}}}},
			map[string]interface{}{
				"Timestamp":   ts,
				"StringArray": []interface{}{"a", "b"},
				"SubStruct":   map[string]interface{}{"String": "c"},
				"SubStructArray": []interface{}{
					map[string]interface{}{"String": "d"},
					map[string]interface{}{"String": "e"},
				},
			},
		},
		{
			"SELECT @val.Timestamp, @val.SubStruct.String",
			[]QueryParameter{{"val", s{Timestamp: ts, SubStruct: ss{"a"}}}},
			[]Value{ts, "a"},
			map[string]interface{}{
				"Timestamp":      ts,
				"SubStruct":      map[string]interface{}{"String": "a"},
				"StringArray":    nil,
				"SubStructArray": nil,
			},
		},
	}
	for _, c := range testCases {
		q := client.Query(c.query)
		q.Parameters = c.parameters
		job, err := q.Run(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if job.LastStatus() == nil {
			t.Error("no LastStatus")
		}
		it, err := job.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		checkRead(t, "QueryParameters", it, [][]Value{c.wantRow})
		config, err := job.Config()
		if err != nil {
			t.Fatal(err)
		}
		got := config.(*QueryConfig).Parameters[0].Value
		if !testutil.Equal(got, c.wantConfig) {
			t.Errorf("param %[1]v (%[1]T): config:\ngot %[2]v (%[2]T)\nwant %[3]v (%[3]T)",
				c.parameters[0].Value, got, c.wantConfig)
		}
	}
}

func TestIntegration_QueryDryRun(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	q := client.Query("SELECT word from " + stdName + " LIMIT 10")
	q.DryRun = true
	job, err := q.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}

	s := job.LastStatus()
	if s.State != Done {
		t.Errorf("state is %v, expected Done", s.State)
	}
	if s.Statistics == nil {
		t.Fatal("no statistics")
	}
	if s.Statistics.Details.(*QueryStatistics).Schema == nil {
		t.Fatal("no schema")
	}
	if s.Statistics.Details.(*QueryStatistics).TotalBytesProcessedAccuracy == "" {
		t.Fatal("no cost accuracy")
	}
}

func TestIntegration_Scripting(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	sql := `
	-- Declare a variable to hold names as an array.
	DECLARE top_names ARRAY<STRING>;
	BEGIN TRANSACTION;
	-- Build an array of the top 100 names from the year 2017.
	SET top_names = (
	  SELECT ARRAY_AGG(name ORDER BY number DESC LIMIT 100)
	  FROM ` + "`bigquery-public-data`" + `.usa_names.usa_1910_current
	  WHERE year = 2017
	);
	-- Which names appear as words in Shakespeare's plays?
	SELECT
	  name AS shakespeare_name
	FROM UNNEST(top_names) AS name
	WHERE name IN (
	  SELECT word
	  FROM ` + "`bigquery-public-data`" + `.samples.shakespeare
	);
	COMMIT TRANSACTION;
	`
	q := client.Query(sql)
	job, err := q.Run(ctx)
	if err != nil {
		t.Fatalf("failed to run parent job: %v", err)
	}
	status, err := job.Wait(ctx)
	if err != nil {
		t.Fatalf("failed to wait for completion: %v", err)
	}
	if status.Err() != nil {
		t.Fatalf("job terminated with error: %v", err)
	}

	queryStats, ok := status.Statistics.Details.(*QueryStatistics)
	if !ok {
		t.Fatalf("failed to fetch query statistics")
	}

	want := "SCRIPT"
	if queryStats.StatementType != want {
		t.Errorf("statement type mismatch. got %s want %s", queryStats.StatementType, want)
	}

	if status.Statistics.NumChildJobs <= 0 {
		t.Errorf("expected script to indicate nonzero child jobs, got %d", status.Statistics.NumChildJobs)
	}

	// Ensure child jobs are present.
	var childJobs []*Job

	it := job.Children(ctx)
	for {
		job, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		childJobs = append(childJobs, job)
	}
	if len(childJobs) == 0 {
		t.Fatal("Script had no child jobs.")
	}

	for _, cj := range childJobs {
		cStatus := cj.LastStatus()
		if cStatus.Statistics.ParentJobID != job.ID() {
			t.Errorf("child job %q doesn't indicate parent.  got %q, want %q", cj.ID(), cStatus.Statistics.ParentJobID, job.ID())
		}
		if cStatus.Statistics.ScriptStatistics == nil {
			t.Errorf("child job %q doesn't have script statistics present", cj.ID())
		}
		if cStatus.Statistics.ScriptStatistics.EvaluationKind == "" {
			t.Errorf("child job %q didn't indicate evaluation kind", cj.ID())
		}
		if cStatus.Statistics.TransactionInfo == nil {
			t.Errorf("child job %q didn't have transaction info present", cj.ID())
		}
		if cStatus.Statistics.TransactionInfo.TransactionID == "" {
			t.Errorf("child job %q didn't have transactionID present", cj.ID())
		}
	}

}

func TestIntegration_ExtractExternal(t *testing.T) {
	// Create a table, extract it to GCS, then query it externally.
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	schema := Schema{
		{Name: "name", Type: StringFieldType},
		{Name: "num", Type: IntegerFieldType},
	}
	table := newTable(t, schema)
	defer table.Delete(ctx)

	// Insert table data.
	sql := fmt.Sprintf(`INSERT %s.%s (name, num)
		                VALUES ('a', 1), ('b', 2), ('c', 3)`,
		table.DatasetID, table.TableID)
	if _, err := runQueryJob(ctx, sql); err != nil {
		t.Fatal(err)
	}
	// Extract to a GCS object as CSV.
	bucketName := testutil.ProjID()
	objectName := fmt.Sprintf("bq-test-%s.csv", table.TableID)
	uri := fmt.Sprintf("gs://%s/%s", bucketName, objectName)
	defer storageClient.Bucket(bucketName).Object(objectName).Delete(ctx)
	gr := NewGCSReference(uri)
	gr.DestinationFormat = CSV
	e := table.ExtractorTo(gr)
	job, err := e.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conf, err := job.Config()
	if err != nil {
		t.Fatal(err)
	}
	config, ok := conf.(*ExtractConfig)
	if !ok {
		t.Fatalf("got %T, want ExtractConfig", conf)
	}
	diff := testutil.Diff(config, &e.ExtractConfig,
		cmp.AllowUnexported(Table{}),
		cmpopts.IgnoreUnexported(Client{}))
	if diff != "" {
		t.Errorf("got=-, want=+:\n%s", diff)
	}
	if err := wait(ctx, job); err != nil {
		t.Fatal(err)
	}

	edc := &ExternalDataConfig{
		SourceFormat: CSV,
		SourceURIs:   []string{uri},
		Schema:       schema,
		Options: &CSVOptions{
			SkipLeadingRows: 1,
			// This is the default. Since we use edc as an expectation later on,
			// let's just be explicit.
			FieldDelimiter: ",",
		},
	}
	// Query that CSV file directly.
	q := client.Query("SELECT * FROM csv")
	q.TableDefinitions = map[string]ExternalData{"csv": edc}
	wantRows := [][]Value{
		{"a", int64(1)},
		{"b", int64(2)},
		{"c", int64(3)},
	}
	iter, err := q.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checkReadAndTotalRows(t, "external query", iter, wantRows)

	// Make a table pointing to the file, and query it.
	// BigQuery does not allow a Table.Read on an external table.
	table = dataset.Table(tableIDs.New())
	err = table.Create(context.Background(), &TableMetadata{
		Schema:             schema,
		ExpirationTime:     testTableExpiration,
		ExternalDataConfig: edc,
	})
	if err != nil {
		t.Fatal(err)
	}
	q = client.Query(fmt.Sprintf("SELECT * FROM %s.%s", table.DatasetID, table.TableID))
	iter, err = q.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	checkReadAndTotalRows(t, "external table", iter, wantRows)

	// While we're here, check that the table metadata is correct.
	md, err := table.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// One difference: since BigQuery returns the schema as part of the ordinary
	// table metadata, it does not populate ExternalDataConfig.Schema.
	md.ExternalDataConfig.Schema = md.Schema
	if diff := testutil.Diff(md.ExternalDataConfig, edc); diff != "" {
		t.Errorf("got=-, want=+\n%s", diff)
	}
}

func TestIntegration_ReadNullIntoStruct(t *testing.T) {
	// Reading a null into a struct field should return an error (not panic).
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	table := newTable(t, schema)
	defer table.Delete(ctx)

	ins := table.Inserter()
	row := &ValuesSaver{
		Schema: schema,
		Row:    []Value{nil, []Value{}, []Value{nil}},
	}
	if err := ins.Put(ctx, []*ValuesSaver{row}); err != nil {
		t.Fatal(putError(err))
	}
	if err := waitForRow(ctx, table); err != nil {
		t.Fatal(err)
	}

	q := client.Query(fmt.Sprintf("select name from %s", table.TableID))
	q.DefaultProjectID = dataset.ProjectID
	q.DefaultDatasetID = dataset.DatasetID
	it, err := q.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	type S struct{ Name string }
	var s S
	if err := it.Next(&s); err == nil {
		t.Fatal("got nil, want error")
	}
}

const (
	stdName    = "`bigquery-public-data.samples.shakespeare`"
	legacyName = "[bigquery-public-data:samples.shakespeare]"
)

// These tests exploit the fact that the two SQL versions have different syntaxes for
// fully-qualified table names.
var useLegacySQLTests = []struct {
	t           string // name of table
	std, legacy bool   // use standard/legacy SQL
	err         bool   // do we expect an error?
}{
	{t: legacyName, std: false, legacy: true, err: false},
	{t: legacyName, std: true, legacy: false, err: true},
	{t: legacyName, std: false, legacy: false, err: true}, // standard SQL is default
	{t: legacyName, std: true, legacy: true, err: true},
	{t: stdName, std: false, legacy: true, err: true},
	{t: stdName, std: true, legacy: false, err: false},
	{t: stdName, std: false, legacy: false, err: false}, // standard SQL is default
	{t: stdName, std: true, legacy: true, err: true},
}

func TestIntegration_QueryUseLegacySQL(t *testing.T) {
	// Test the UseLegacySQL and UseStandardSQL options for queries.
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	for _, test := range useLegacySQLTests {
		q := client.Query(fmt.Sprintf("select word from %s limit 1", test.t))
		q.UseStandardSQL = test.std
		q.UseLegacySQL = test.legacy
		_, err := q.Read(ctx)
		gotErr := err != nil
		if gotErr && !test.err {
			t.Errorf("%+v:\nunexpected error: %v", test, err)
		} else if !gotErr && test.err {
			t.Errorf("%+v:\nsucceeded, but want error", test)
		}
	}
}

func TestIntegration_TableUseLegacySQL(t *testing.T) {
	// Test UseLegacySQL and UseStandardSQL for Table.Create.
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	table := newTable(t, schema)
	defer table.Delete(ctx)
	for i, test := range useLegacySQLTests {
		view := dataset.Table(fmt.Sprintf("t_view_%d", i))
		tm := &TableMetadata{
			ViewQuery:      fmt.Sprintf("SELECT word from %s", test.t),
			UseStandardSQL: test.std,
			UseLegacySQL:   test.legacy,
		}
		err := view.Create(ctx, tm)
		gotErr := err != nil
		if gotErr && !test.err {
			t.Errorf("%+v:\nunexpected error: %v", test, err)
		} else if !gotErr && test.err {
			t.Errorf("%+v:\nsucceeded, but want error", test)
		}
		_ = view.Delete(ctx)
	}
}

func TestIntegration_ListJobs(t *testing.T) {
	// It's difficult to test the list of jobs, because we can't easily
	// control what's in it. Also, there are many jobs in the test project,
	// and it takes considerable time to list them all.
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()

	// About all we can do is list a few jobs.
	const max = 20
	var jobs []*Job
	it := client.Jobs(ctx)
	for {
		job, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, job)
		if len(jobs) >= max {
			break
		}
	}
	// We expect that there is at least one job in the last few months.
	if len(jobs) == 0 {
		t.Fatal("did not get any jobs")
	}
}

func TestIntegration_DeleteJob(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()

	q := client.Query("SELECT 17 as foo")
	q.Location = "us-east1"

	job, err := q.Run(ctx)
	if err != nil {
		t.Fatalf("job Run failure: %v", err)
	}
	_, err = job.Wait(ctx)
	if err != nil {
		t.Fatalf("job completion failure: %v", err)
	}

	if err := job.Delete(ctx); err != nil {
		t.Fatalf("job.Delete failed: %v", err)
	}
}

const tokyo = "asia-northeast1"

func TestIntegration_Location(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	client.Location = ""
	testLocation(t, tokyo)
	client.Location = tokyo
	defer func() {
		client.Location = ""
	}()
	testLocation(t, "")
}

func testLocation(t *testing.T, loc string) {
	ctx := context.Background()
	tokyoDataset := client.Dataset("tokyo")
	err := tokyoDataset.Create(ctx, &DatasetMetadata{Location: loc})
	if err != nil && !hasStatusCode(err, 409) { // 409 = already exists
		t.Fatal(err)
	}
	md, err := tokyoDataset.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if md.Location != tokyo {
		t.Fatalf("dataset location: got %s, want %s", md.Location, tokyo)
	}
	table := tokyoDataset.Table(tableIDs.New())
	err = table.Create(context.Background(), &TableMetadata{
		Schema: Schema{
			{Name: "name", Type: StringFieldType},
			{Name: "nums", Type: IntegerFieldType},
		},
		ExpirationTime: testTableExpiration,
	})
	if err != nil {
		t.Fatal(err)
	}

	tableMetadata, err := table.Metadata(ctx)
	if err != nil {
		t.Fatalf("failed to get table metadata: %v", err)
	}
	wantLoc := loc
	if loc == "" && client.Location != "" {
		wantLoc = client.Location
	}
	if tableMetadata.Location != wantLoc {
		t.Errorf("Location on table doesn't match.  Got %s want %s", tableMetadata.Location, wantLoc)
	}
	defer table.Delete(ctx)
	loader := table.LoaderFrom(NewReaderSource(strings.NewReader("a,0\nb,1\nc,2\n")))
	loader.Location = loc
	job, err := loader.Run(ctx)
	if err != nil {
		t.Fatal("loader.Run", err)
	}
	if job.Location() != tokyo {
		t.Fatalf("job location: got %s, want %s", job.Location(), tokyo)
	}
	_, err = client.JobFromID(ctx, job.ID())
	if client.Location == "" && err == nil {
		t.Error("JobFromID with Tokyo job, no client location: want error, got nil")
	}
	if client.Location != "" && err != nil {
		t.Errorf("JobFromID with Tokyo job, with client location: want nil, got %v", err)
	}
	_, err = client.JobFromIDLocation(ctx, job.ID(), "US")
	if err == nil {
		t.Error("JobFromIDLocation with US: want error, got nil")
	}
	job2, err := client.JobFromIDLocation(ctx, job.ID(), loc)
	if loc == tokyo && err != nil {
		t.Errorf("loc=tokyo: %v", err)
	}
	if loc == "" && err == nil {
		t.Error("loc empty: got nil, want error")
	}
	if job2 != nil && (job2.ID() != job.ID() || job2.Location() != tokyo) {
		t.Errorf("got id %s loc %s, want id%s loc %s", job2.ID(), job2.Location(), job.ID(), tokyo)
	}
	if err := wait(ctx, job); err != nil {
		t.Fatal(err)
	}
	// Cancel should succeed even if the job is done.
	if err := job.Cancel(ctx); err != nil {
		t.Fatal(err)
	}

	q := client.Query(fmt.Sprintf("SELECT * FROM %s.%s", table.DatasetID, table.TableID))
	q.Location = loc
	iter, err := q.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantRows := [][]Value{
		{"a", int64(0)},
		{"b", int64(1)},
		{"c", int64(2)},
	}
	checkRead(t, "location", iter, wantRows)

	table2 := tokyoDataset.Table(tableIDs.New())
	copier := table2.CopierFrom(table)
	copier.Location = loc
	if _, err := copier.Run(ctx); err != nil {
		t.Fatal(err)
	}
	bucketName := testutil.ProjID()
	objectName := fmt.Sprintf("bq-test-%s.csv", table.TableID)
	uri := fmt.Sprintf("gs://%s/%s", bucketName, objectName)
	defer storageClient.Bucket(bucketName).Object(objectName).Delete(ctx)
	gr := NewGCSReference(uri)
	gr.DestinationFormat = CSV
	e := table.ExtractorTo(gr)
	e.Location = loc
	if _, err := e.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_NumericErrors(t *testing.T) {
	// Verify that the service returns an error for a big.Rat that's too large.
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	schema := Schema{{Name: "n", Type: NumericFieldType}}
	table := newTable(t, schema)
	defer table.Delete(ctx)
	tooBigRat := &big.Rat{}
	if _, ok := tooBigRat.SetString("1e40"); !ok {
		t.Fatal("big.Rat.SetString failed")
	}
	ins := table.Inserter()
	err := ins.Put(ctx, []*ValuesSaver{{Schema: schema, Row: []Value{tooBigRat}}})
	if err == nil {
		t.Fatal("got nil, want error")
	}
}

func TestIntegration_QueryErrors(t *testing.T) {
	// Verify that a bad query returns an appropriate error.
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()
	q := client.Query("blah blah broken")
	_, err := q.Read(ctx)
	const want = "invalidQuery"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("got %q, want substring %q", err, want)
	}
}

func TestIntegration_MaterializedViewLifecycle(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()

	// instantiate a base table via a CTAS
	baseTableID := tableIDs.New()
	qualified := fmt.Sprintf("`%s`.%s.%s", testutil.ProjID(), dataset.DatasetID, baseTableID)
	sql := fmt.Sprintf(`
	CREATE TABLE %s
	(
		sample_value INT64,
		groupid STRING,
	)
	AS
	SELECT
	  CAST(RAND() * 100 AS INT64),
	  CONCAT("group", CAST(CAST(RAND()*10 AS INT64) AS STRING))
	FROM
	  UNNEST(GENERATE_ARRAY(0,999))
	`, qualified)
	if _, err := runQueryJob(ctx, sql); err != nil {
		t.Fatalf("couldn't instantiate base table: %v", err)
	}

	// Define the SELECT aggregation to become a mat view
	sql = fmt.Sprintf(`
	SELECT
	  SUM(sample_value) as total,
	  groupid
	FROM
	  %s
	GROUP BY groupid
	`, qualified)

	// Create materialized view

	wantRefresh := 6 * time.Hour
	matViewID := tableIDs.New()
	view := dataset.Table(matViewID)
	if err := view.Create(ctx, &TableMetadata{
		MaterializedView: &MaterializedViewDefinition{
			Query:           sql,
			RefreshInterval: wantRefresh,
		}}); err != nil {
		t.Fatal(err)
	}

	// Get metadata
	curMeta, err := view.Metadata(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if curMeta.MaterializedView == nil {
		t.Fatal("expected materialized view definition, was null")
	}

	if curMeta.MaterializedView.Query != sql {
		t.Errorf("mismatch on view sql.  Got %s want %s", curMeta.MaterializedView.Query, sql)
	}

	if curMeta.MaterializedView.RefreshInterval != wantRefresh {
		t.Errorf("mismatch on refresh time: got %d usec want %d usec", 1000*curMeta.MaterializedView.RefreshInterval.Nanoseconds(), 1000*wantRefresh.Nanoseconds())
	}

	// MaterializedView is a TableType constant
	want := MaterializedView
	if curMeta.Type != want {
		t.Errorf("mismatch on table type.  got %s want %s", curMeta.Type, want)
	}

	// Update metadata
	wantRefresh = time.Hour // 6hr -> 1hr
	upd := TableMetadataToUpdate{
		MaterializedView: &MaterializedViewDefinition{
			Query:           sql,
			RefreshInterval: wantRefresh,
		},
	}

	newMeta, err := view.Update(ctx, upd, curMeta.ETag)
	if err != nil {
		t.Fatalf("failed to update view definition: %v", err)
	}

	if newMeta.MaterializedView == nil {
		t.Error("MaterializeView missing in updated metadata")
	}

	if newMeta.MaterializedView.RefreshInterval != wantRefresh {
		t.Errorf("mismatch on updated refresh time: got %d usec want %d usec", 1000*curMeta.MaterializedView.RefreshInterval.Nanoseconds(), 1000*wantRefresh.Nanoseconds())
	}

	// verify implicit setting of false due to partial population of update.
	if newMeta.MaterializedView.EnableRefresh {
		t.Error("expected EnableRefresh to be false, is true")
	}

	// Verify list

	it := dataset.Tables(ctx)
	seen := false
	for {
		tbl, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if tbl.TableID == matViewID {
			seen = true
		}
	}
	if !seen {
		t.Error("materialized view not listed in dataset")
	}

	// Verify deletion
	if err := view.Delete(ctx); err != nil {
		t.Errorf("failed to delete materialized view: %v", err)
	}

}

func TestIntegration_ModelLifecycle(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()

	// Create a model via a CREATE MODEL query
	modelID := modelIDs.New()
	model := dataset.Model(modelID)
	modelRef := fmt.Sprintf("%s.%s.%s", dataset.ProjectID, dataset.DatasetID, modelID)

	sql := fmt.Sprintf(`
		CREATE MODEL `+"`%s`"+`
		OPTIONS (
			model_type='linear_reg',
			max_iteration=1,
			learn_rate=0.4,
			learn_rate_strategy='constant'
		) AS (
			SELECT 'a' AS f1, 2.0 AS label
			UNION ALL
			SELECT 'b' AS f1, 3.8 AS label
		)`, modelRef)
	if _, err := runQueryJob(ctx, sql); err != nil {
		t.Fatal(err)
	}
	defer model.Delete(ctx)

	// Get the model metadata.
	curMeta, err := model.Metadata(ctx)
	if err != nil {
		t.Fatalf("couldn't get metadata: %v", err)
	}

	want := "LINEAR_REGRESSION"
	if curMeta.Type != want {
		t.Errorf("Model type mismatch.  Want %s got %s", curMeta.Type, want)
	}

	// Ensure training metadata is available.
	runs := curMeta.RawTrainingRuns()
	if runs == nil {
		t.Errorf("training runs unpopulated.")
	}
	labelCols, err := curMeta.RawLabelColumns()
	if err != nil {
		t.Fatalf("failed to get label cols: %v", err)
	}
	if labelCols == nil {
		t.Errorf("label column information unpopulated.")
	}
	featureCols, err := curMeta.RawFeatureColumns()
	if err != nil {
		t.Fatalf("failed to get feature cols: %v", err)
	}
	if featureCols == nil {
		t.Errorf("feature column information unpopulated.")
	}

	// Update mutable fields via API.
	expiry := time.Now().Add(24 * time.Hour).Truncate(time.Millisecond)

	upd := ModelMetadataToUpdate{
		Description:    "new",
		Name:           "friendly",
		ExpirationTime: expiry,
	}

	newMeta, err := model.Update(ctx, upd, curMeta.ETag)
	if err != nil {
		t.Fatalf("failed to update: %v", err)
	}

	want = "new"
	if newMeta.Description != want {
		t.Fatalf("Description not updated. got %s want %s", newMeta.Description, want)
	}
	want = "friendly"
	if newMeta.Name != want {
		t.Fatalf("Description not updated. got %s want %s", newMeta.Description, want)
	}
	if newMeta.ExpirationTime != expiry {
		t.Fatalf("ExpirationTime not updated.  got %v want %v", newMeta.ExpirationTime, expiry)
	}

	// Ensure presence when enumerating the model list.
	it := dataset.Models(ctx)
	seen := false
	for {
		mdl, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if mdl.ModelID == modelID {
			seen = true
		}
	}
	if !seen {
		t.Fatal("model not listed in dataset")
	}

	// Extract the model to GCS.
	bucketName := testutil.ProjID()
	objectName := fmt.Sprintf("bq-model-extract-%s", modelID)
	uri := fmt.Sprintf("gs://%s/%s", bucketName, objectName)
	defer storageClient.Bucket(bucketName).Object(objectName).Delete(ctx)
	gr := NewGCSReference(uri)
	gr.DestinationFormat = TFSavedModel
	extractor := model.ExtractorTo(gr)
	job, err := extractor.Run(ctx)
	if err != nil {
		t.Fatalf("failed to extract model to GCS: %v", err)
	}
	if _, err := job.Wait(ctx); err != nil {
		t.Errorf("failed to complete extract job (%s): %v", job.ID(), err)
	}

	// Delete the model.
	if err := model.Delete(ctx); err != nil {
		t.Fatalf("failed to delete model: %v", err)
	}
}

func TestIntegration_RoutineScalarUDF(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()

	// Create a scalar UDF routine via API.
	routineID := routineIDs.New()
	routine := dataset.Routine(routineID)
	err := routine.Create(ctx, &RoutineMetadata{
		Type:     "SCALAR_FUNCTION",
		Language: "SQL",
		Body:     "x * 3",
		Arguments: []*RoutineArgument{
			{
				Name: "x",
				DataType: &StandardSQLDataType{
					TypeKind: "INT64",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
}

func TestIntegration_RoutineJSUDF(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()

	// Create a scalar UDF routine via API.
	routineID := routineIDs.New()
	routine := dataset.Routine(routineID)
	meta := &RoutineMetadata{
		Language: "JAVASCRIPT", Type: "SCALAR_FUNCTION",
		Description:      "capitalizes using javascript",
		DeterminismLevel: Deterministic,
		Arguments: []*RoutineArgument{
			{Name: "instr", Kind: "FIXED_TYPE", DataType: &StandardSQLDataType{TypeKind: "STRING"}},
		},
		ReturnType: &StandardSQLDataType{TypeKind: "STRING"},
		Body:       "return instr.toUpperCase();",
	}
	if err := routine.Create(ctx, meta); err != nil {
		t.Fatalf("Create: %v", err)
	}

	newMeta := &RoutineMetadataToUpdate{
		Language:    meta.Language,
		Body:        meta.Body,
		Arguments:   meta.Arguments,
		Description: meta.Description,
		ReturnType:  meta.ReturnType,
		Type:        meta.Type,

		DeterminismLevel: NotDeterministic,
	}
	if _, err := routine.Update(ctx, newMeta, ""); err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestIntegration_RoutineComplexTypes(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()

	routineID := routineIDs.New()
	routine := dataset.Routine(routineID)
	sql := fmt.Sprintf(`
		CREATE FUNCTION `+"`%s`("+`
			arr ARRAY<STRUCT<name STRING, val INT64>>
		  ) AS (
			  (SELECT SUM(IF(elem.name = "foo",elem.val,null)) FROM UNNEST(arr) AS elem)
		  )`,
		routine.FullyQualifiedName())
	if _, err := runQueryJob(ctx, sql); err != nil {
		t.Fatal(err)
	}
	defer routine.Delete(ctx)

	meta, err := routine.Metadata(ctx)
	if err != nil {
		t.Fatalf("Metadata: %v", err)
	}
	if meta.Type != "SCALAR_FUNCTION" {
		t.Fatalf("routine type mismatch, got %s want SCALAR_FUNCTION", meta.Type)
	}
	if meta.Language != "SQL" {
		t.Fatalf("language type mismatch, got  %s want SQL", meta.Language)
	}
	want := []*RoutineArgument{
		{
			Name: "arr",
			DataType: &StandardSQLDataType{
				TypeKind: "ARRAY",
				ArrayElementType: &StandardSQLDataType{
					TypeKind: "STRUCT",
					StructType: &StandardSQLStructType{
						Fields: []*StandardSQLField{
							{
								Name: "name",
								Type: &StandardSQLDataType{
									TypeKind: "STRING",
								},
							},
							{
								Name: "val",
								Type: &StandardSQLDataType{
									TypeKind: "INT64",
								},
							},
						},
					},
				},
			},
		},
	}
	if diff := testutil.Diff(meta.Arguments, want); diff != "" {
		t.Fatalf("%+v: -got, +want:\n%s", meta.Arguments, diff)
	}
}

func TestIntegration_RoutineLifecycle(t *testing.T) {
	if client == nil {
		t.Skip("Integration tests skipped")
	}
	ctx := context.Background()

	// Create a scalar UDF routine via a CREATE FUNCTION query
	routineID := routineIDs.New()
	routine := dataset.Routine(routineID)

	sql := fmt.Sprintf(`
		CREATE FUNCTION `+"`%s`"+`(x INT64) AS (x * 3);`,
		routine.FullyQualifiedName())
	if _, err := runQueryJob(ctx, sql); err != nil {
		t.Fatal(err)
	}
	defer routine.Delete(ctx)

	// Get the routine metadata.
	curMeta, err := routine.Metadata(ctx)
	if err != nil {
		t.Fatalf("couldn't get metadata: %v", err)
	}

	want := "SCALAR_FUNCTION"
	if curMeta.Type != want {
		t.Errorf("Routine type mismatch.  got %s want %s", curMeta.Type, want)
	}

	want = "SQL"
	if curMeta.Language != want {
		t.Errorf("Language mismatch. got %s want %s", curMeta.Language, want)
	}

	// Perform an update to change the routine body and description.
	want = "x * 4"
	wantDescription := "an updated description"
	// during beta, update doesn't allow partial updates.  Provide all fields.
	newMeta, err := routine.Update(ctx, &RoutineMetadataToUpdate{
		Body:        want,
		Arguments:   curMeta.Arguments,
		Description: wantDescription,
		ReturnType:  curMeta.ReturnType,
		Type:        curMeta.Type,
	}, curMeta.ETag)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if newMeta.Body != want {
		t.Fatalf("Update body failed. want %s got %s", want, newMeta.Body)
	}
	if newMeta.Description != wantDescription {
		t.Fatalf("Update description failed. want %s got %s", wantDescription, newMeta.Description)
	}

	// Ensure presence when enumerating the model list.
	it := dataset.Routines(ctx)
	seen := false
	for {
		r, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if r.RoutineID == routineID {
			seen = true
		}
	}
	if !seen {
		t.Fatal("routine not listed in dataset")
	}

	// Delete the model.
	if err := routine.Delete(ctx); err != nil {
		t.Fatalf("failed to delete routine: %v", err)
	}
}

// Creates a new, temporary table with a unique name and the given schema.
func newTable(t *testing.T, s Schema) *Table {
	table := dataset.Table(tableIDs.New())
	err := table.Create(context.Background(), &TableMetadata{
		Schema:         s,
		ExpirationTime: testTableExpiration,
	})
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func checkRead(t *testing.T, msg string, it *RowIterator, want [][]Value) {
	if msg2, ok := compareRead(it, want, false); !ok {
		t.Errorf("%s: %s", msg, msg2)
	}
}

func checkReadAndTotalRows(t *testing.T, msg string, it *RowIterator, want [][]Value) {
	if msg2, ok := compareRead(it, want, true); !ok {
		t.Errorf("%s: %s", msg, msg2)
	}
}

func compareRead(it *RowIterator, want [][]Value, compareTotalRows bool) (msg string, ok bool) {
	got, _, totalRows, err := readAll(it)
	if err != nil {
		return err.Error(), false
	}
	if len(got) != len(want) {
		return fmt.Sprintf("got %d rows, want %d", len(got), len(want)), false
	}
	if compareTotalRows && len(got) != int(totalRows) {
		return fmt.Sprintf("got %d rows, but totalRows = %d", len(got), totalRows), false
	}
	sort.Sort(byCol0(got))
	for i, r := range got {
		gotRow := []Value(r)
		wantRow := want[i]
		if !testutil.Equal(gotRow, wantRow) {
			return fmt.Sprintf("#%d: got %#v, want %#v", i, gotRow, wantRow), false
		}
	}
	return "", true
}

func readAll(it *RowIterator) ([][]Value, Schema, uint64, error) {
	var (
		rows      [][]Value
		schema    Schema
		totalRows uint64
	)
	for {
		var vals []Value
		err := it.Next(&vals)
		if err == iterator.Done {
			return rows, schema, totalRows, nil
		}
		if err != nil {
			return nil, nil, 0, err
		}
		rows = append(rows, vals)
		schema = it.Schema
		totalRows = it.TotalRows
	}
}

type byCol0 [][]Value

func (b byCol0) Len() int      { return len(b) }
func (b byCol0) Swap(i, j int) { b[i], b[j] = b[j], b[i] }
func (b byCol0) Less(i, j int) bool {
	switch a := b[i][0].(type) {
	case string:
		return a < b[j][0].(string)
	case civil.Date:
		return a.Before(b[j][0].(civil.Date))
	default:
		panic("unknown type")
	}
}

func hasStatusCode(err error, code int) bool {
	if e, ok := err.(*googleapi.Error); ok && e.Code == code {
		return true
	}
	return false
}

// wait polls the job until it is complete or an error is returned.
func wait(ctx context.Context, job *Job) error {
	status, err := job.Wait(ctx)
	if err != nil {
		return err
	}
	if status.Err() != nil {
		return fmt.Errorf("job status error: %#v", status.Err())
	}
	if status.Statistics == nil {
		return errors.New("nil Statistics")
	}
	if status.Statistics.EndTime.IsZero() {
		return errors.New("EndTime is zero")
	}
	if status.Statistics.Details == nil {
		return errors.New("nil Statistics.Details")
	}
	return nil
}

// waitForRow polls the table until it contains a row.
// TODO(jba): use internal.Retry.
func waitForRow(ctx context.Context, table *Table) error {
	for {
		it := table.Read(ctx)
		var v []Value
		err := it.Next(&v)
		if err == nil {
			return nil
		}
		if err != iterator.Done {
			return err
		}
		time.Sleep(1 * time.Second)
	}
}

func putError(err error) string {
	pme, ok := err.(PutMultiError)
	if !ok {
		return err.Error()
	}
	var msgs []string
	for _, err := range pme {
		msgs = append(msgs, err.Error())
	}
	return strings.Join(msgs, "\n")
}
