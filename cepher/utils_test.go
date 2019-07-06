package main

import (
	"fmt"
	"testing"
	"time"
)

func TestExecuteCommand(t *testing.T) {
	test, err := ExecShellTimeout(5*time.Second, "lsg")
	if err != nil {
		fmt.Println("Error --> ", err)
	} else {
		fmt.Println("Executed Command")
		fmt.Println(test)
	}
	test2, err2 := ShWithTimeout(5*time.Second, "lsg")
	if err2 != nil {
		fmt.Println("Error --> ", err2)
	} else {
		fmt.Println("Executed Command")
		fmt.Println(test2)
	}
}

func TestExecuteCommandDefaultTimeout(t *testing.T) {
	test, err := shWithDefaultTimeout("curl", "http://localhost:3001/teste", "-H", "'Authorization: Bearer token'")
	if err != nil {
		fmt.Println("Error --> ", err)
	} else {
		fmt.Println("Executed Command")
		fmt.Println(test)
	}
}

func TestGenerateImageBackupName(t *testing.T) {
	type args struct {
		name     string
		nameList []string
	}
	tests := []struct {
		name    string
		args    args
		want    string
		wantErr bool
	}{
		{
			name: "first time backup name",
			args: args{
				"image-name",
				[]string{"trash_0_another-image-name", "image-name"},
			},
			want:    "trash_0_image-name",
			wantErr: false,
		},
		{
			name: "nil image names",
			args: args{
				"image-name",
				nil,
			},
			want:    "trash_0_image-name",
			wantErr: false,
		},
		{
			name: "second backup name",
			args: args{
				"image-name",
				[]string{"trash_0_image-name"},
			},
			want:    "trash_1_image-name",
			wantErr: false,
		},
		{
			name: "multiples existent backups",
			args: args{
				"image-name",
				[]string{"image-name", "trash_0_image-name", "trash_0_other-name", "trash_1_image-name", "trash_2_image-name", "trash_3_image-name", "trash_3_other-name"},
			},
			want:    "trash_4_image-name",
			wantErr: false,
		},
		{
			name: "always increment the counter from the highest number founded",
			args: args{
				"image-name",
				[]string{"trash_22_image-name", "trash_17_image-name", "trash_3_image-name", "trash_18_image-name"},
			},
			want:    "trash_23_image-name",
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := GenerateImageBackupName(tt.args.name, tt.args.nameList)
			if (err != nil) != tt.wantErr {
				t.Errorf("GenerateImageBackupName() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("GenerateImageBackupName() = %v, want %v", got, tt.want)
			}
		})
	}
}
