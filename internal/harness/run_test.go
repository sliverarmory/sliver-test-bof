package harness

import (
	"strings"
	"testing"

	"github.com/bishopfox/sliver/protobuf/clientpb"
	"github.com/bishopfox/sliver/protobuf/commonpb"
	"github.com/bishopfox/sliver/protobuf/sliverpb"
	"google.golang.org/protobuf/proto"
)

func TestDecodeBeaconTaskRequiresExactCompletedTask(t *testing.T) {
	responseData, err := proto.Marshal(&sliverpb.CallExtension{
		Output:   []byte("ok"),
		Response: &commonpb.Response{},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := &clientpb.BeaconTask{
		ID: "task-1", BeaconID: "beacon-1", State: "completed", CompletedAt: 1, Response: responseData,
	}
	destination := &sliverpb.CallExtension{}
	if err := decodeBeaconTask(task, "task-1", "beacon-1", destination); err != nil {
		t.Fatal(err)
	}
	if string(destination.Output) != "ok" || destination.Response == nil {
		t.Fatalf("decoded response = %#v", destination)
	}
	if !beaconTaskCompleted(task, "task-1", "beacon-1") {
		t.Fatal("completed task was not recognized")
	}

	task.BeaconID = "other"
	if err := decodeBeaconTask(task, "task-1", "beacon-1", destination); err == nil || !strings.Contains(err.Error(), "correlation mismatch") {
		t.Fatalf("correlation error = %v", err)
	}
	if beaconTaskCompleted(task, "task-1", "beacon-1") {
		t.Fatal("mismatched task was recognized as completed")
	}
}
