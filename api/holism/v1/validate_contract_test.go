package holismv1

import "testing"

func TestTaskRequiresProjectID(t *testing.T) {
	if err := (&Task{}).Validate(); err == nil {
		t.Fatal("expected Task without project_id to fail validation")
	}
	if err := (&Task{ProjectId: "p-1"}).Validate(); err != nil {
		t.Fatalf("expected Task with project_id to pass: %v", err)
	}
}

func TestSubmitTaskRequiresProjectID(t *testing.T) {
	if err := (&SubmitTaskRequest{}).Validate(); err == nil {
		t.Fatal("expected SubmitTaskRequest without project_id to fail validation")
	}
}

func TestListTasksRequiresProjectID(t *testing.T) {
	if err := (&ListTasksRequest{}).Validate(); err == nil {
		t.Fatal("expected ListTasksRequest without project_id to fail validation")
	}
}
