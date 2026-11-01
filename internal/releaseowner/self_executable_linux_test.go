package releaseowner

import "testing"

func TestNativeSelfCapabilityClosesWithSessionButPreservesBorrowedImage(t *testing.T) {
	image, err := retainSelfExecutable()
	if err != nil {
		t.Fatal(err)
	}
	borrowed, digest, err := image.Borrow()
	if err != nil {
		t.Fatal(err)
	}
	defer borrowed.Close()
	session := session{self: image}
	session.close()
	if _, _, err := image.Borrow(); err == nil {
		t.Fatal("closed session minted fresh executable custody")
	}
	if _, err := borrowed.Stat(); err != nil || !fingerprint(digest) {
		t.Fatal("session close revoked already borrowed image", err)
	}
}
