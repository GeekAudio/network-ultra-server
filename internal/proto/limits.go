package proto

// Protocol v1 input and roster limits. These values are shared by validation,
// storage, and bcrypt gates so no layer can accidentally accept a payload that
// another v1 client cannot represent safely.
const (
	MaxPeersPerRoom  = 8
	MaxUsernameRunes = 32
	MaxRoomNameRunes = 64
	MaxPasswordBytes = 72 // bcrypt's defined input boundary
)
