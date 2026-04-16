package vmware_nbdkit

import (
	"strings"
	"testing"
)

func TestBuildV2VDomainXML_SingleDisk(t *testing.T) {
	xml, err := buildV2VDomainXML("test-vm", []string{"/dev/vda"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(xml, "<name>test-vm</name>") {
		t.Error("expected VM name in XML")
	}
	if !strings.Contains(xml, "<source dev='/dev/vda'/>") {
		t.Error("expected disk source path in XML")
	}
	if !strings.Contains(xml, "<target dev='vda' bus='virtio'/>") {
		t.Error("expected target dev 'vda' in XML")
	}
	if count := strings.Count(xml, "<disk "); count != 1 {
		t.Errorf("expected 1 disk element, got %d", count)
	}
}

func TestBuildV2VDomainXML_MultiDisk(t *testing.T) {
	paths := []string{"/dev/sda", "/dev/sdb", "/dev/sdc"}
	xml, err := buildV2VDomainXML("multi-disk-vm", paths)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !strings.Contains(xml, "<name>multi-disk-vm</name>") {
		t.Error("expected VM name in XML")
	}

	if count := strings.Count(xml, "<disk "); count != 3 {
		t.Errorf("expected 3 disk elements, got %d", count)
	}

	for i, p := range paths {
		devName := string(rune('a' + i))
		if !strings.Contains(xml, "<source dev='"+p+"'/>") {
			t.Errorf("expected source dev '%s' in XML", p)
		}
		if !strings.Contains(xml, "<target dev='vd"+devName+"' bus='virtio'/>") {
			t.Errorf("expected target dev 'vd%s' in XML", devName)
		}
	}
}

func TestBuildV2VDomainXML_EmptyPaths(t *testing.T) {
	_, err := buildV2VDomainXML("empty-vm", []string{})
	if err == nil {
		t.Error("expected error for empty paths, got nil")
	}
}

func TestBuildV2VDomainXML_TooManyDisks(t *testing.T) {
	paths := make([]string, 27)
	for i := range paths {
		paths[i] = "/dev/sdx"
	}
	_, err := buildV2VDomainXML("big-vm", paths)
	if err == nil {
		t.Error("expected error for >26 disks, got nil")
	}
}

func TestBuildV2VDomainXML_UniqueDevNames(t *testing.T) {
	paths := []string{"/dev/sda", "/dev/sdb"}
	xml, err := buildV2VDomainXML("two-disk-vm", paths)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if strings.Count(xml, "dev='vda'") != 1 {
		t.Error("expected exactly one 'vda' target dev")
	}
	if strings.Count(xml, "dev='vdb'") != 1 {
		t.Error("expected exactly one 'vdb' target dev")
	}
}
