package stdio

type StreamType byte

const (
	StreamStdin  StreamType = 0x00
	StreamStdout StreamType = 0x01
	StreamStderr StreamType = 0x02
)

func Wrap(streamType StreamType, data []byte) []byte {
	out := make([]byte, 1+len(data))
	out[0] = byte(streamType)
	copy(out[1:], data)
	return out
}

func Unwrap(data []byte) (StreamType, []byte) {
	if len(data) == 0 {
		return 0, nil
	}
	return StreamType(data[0]), data[1:]
}
