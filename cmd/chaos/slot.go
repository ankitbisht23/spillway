package main

import "strings"

// slots is Redis Cluster's hash slot count.
const slots = 16384

// keySlot is the hash slot of a Redis key: CRC16 of its hash tag, the text
// between the first { and the next }, when that is not empty, else of the
// whole key, modulo 16384 (Redis Cluster specification).
func keySlot(key string) int {
	if i := strings.IndexByte(key, '{'); i >= 0 {
		if j := strings.IndexByte(key[i+1:], '}'); j > 0 {
			key = key[i+1 : i+1+j]
		}
	}
	return int(crc16(key)) % slots
}

// crc16 is CRC-16/XMODEM (polynomial 0x1021, initial value 0), the CRC Redis
// Cluster hashes keys with.
func crc16(s string) uint16 {
	var crc uint16
	for i := range len(s) {
		crc ^= uint16(s[i]) << 8
		for range 8 {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
	}
	return crc
}
