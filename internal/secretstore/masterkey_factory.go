package secretstore

// NewPlatformMasterKeySource returns the native master-key source for the
// current operating system. vaultPath is used only to locate platform metadata
// next to the encrypted vault; it is never used to store a plaintext key.
func NewPlatformMasterKeySource(vaultPath string) MasterKeySource {
	return newPlatformMasterKeySource(vaultPath)
}
