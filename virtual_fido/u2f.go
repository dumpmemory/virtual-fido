package virtual_fido

import (
	"bytes"
	"crypto/elliptic"
	"crypto/x509"
	"fmt"

	util "github.com/bulwarkid/virtual-fido/virtual_fido/util"
	"github.com/fxamacker/cbor/v2"
)

var u2fLogger = newLogger("[U2F] ", false)

type u2fCommand uint8

const (
	u2f_COMMAND_REGISTER     u2fCommand = 0x01
	u2f_COMMAND_AUTHENTICATE u2fCommand = 0x02
	u2f_COMMAND_VERSION      u2fCommand = 0x03
)

var u2fCommandDescriptions = map[u2fCommand]string{
	u2f_COMMAND_REGISTER:     "u2f_COMMAND_REGISTER",
	u2f_COMMAND_AUTHENTICATE: "u2f_COMMAND_AUTHENTICATE",
	u2f_COMMAND_VERSION:      "u2f_COMMAND_VERSION",
}

type u2fStatusWord uint16

const (
	u2f_SW_NO_ERROR                 u2fStatusWord = 0x9000
	u2f_SW_CONDITIONS_NOT_SATISFIED u2fStatusWord = 0x6985
	u2f_SW_WRONG_DATA               u2fStatusWord = 0x6A80
	u2f_SW_WRONG_LENGTH             u2fStatusWord = 0x6700
	u2f_SW_CLA_NOT_SUPPORTED        u2fStatusWord = 0x6E00
	u2f_SW_INS_NOT_SUPPORTED        u2fStatusWord = 0x6D00
)

type u2fAuthenticateControl uint8

const (
	u2f_AUTH_CONTROL_CHECK_ONLY                     u2fAuthenticateControl = 0x07
	u2f_AUTH_CONTROL_ENFORCE_USER_PRESENCE_AND_SIGN u2fAuthenticateControl = 0x03
	u2f_AUTH_CONTROL_SIGN                           u2fAuthenticateControl = 0x08
)

type u2fMessageHeader struct {
	Cla     uint8
	Command u2fCommand
	Param1  uint8
	Param2  uint8
}

func (header u2fMessageHeader) String() string {
	return fmt.Sprintf("u2fMessageHeader{ Cla: 0x%x, Command: %s, Param1: %d, Param2: %d }",
		header.Cla,
		u2fCommandDescriptions[header.Command],
		header.Param1,
		header.Param2)
}

type u2fServer struct {
	client FIDOClient
}

func newU2FServer(client FIDOClient) *u2fServer {
	return &u2fServer{client: client}
}

func decodeU2FMessage(messageBytes []byte) (u2fMessageHeader, []byte, uint16) {
	buffer := bytes.NewBuffer(messageBytes)
	header := util.ReadBE[u2fMessageHeader](buffer)
	if buffer.Len() == 0 {
		// No request length, no response length
		return header, []byte{}, 0
	}
	// We should either have a request length or response length, so we have at least
	// one '0' byte at the start
	if util.Read(buffer, 1)[0] != 0 {
		panic(fmt.Sprintf("Invalid U2F Payload length: %s %#v", header, messageBytes))
	}
	length := util.ReadBE[uint16](buffer)
	if buffer.Len() == 0 {
		// No payload, so length must be the response length
		return header, []byte{}, length
	}
	// length is the request length
	request := util.Read(buffer, uint(length))
	if buffer.Len() == 0 {
		return header, request, 0
	}
	responseLength := util.ReadBE[uint16](buffer)
	return header, request, responseLength
}

func (server *u2fServer) handleU2FMessage(message []byte) []byte {
	header, request, responseLength := decodeU2FMessage(message)
	u2fLogger.Printf("U2F MESSAGE: Header: %s Request: %#v Response Length: %d\n\n", header, request, responseLength)
	var response []byte
	switch header.Command {
	case u2f_COMMAND_VERSION:
		response = append([]byte("U2F_V2"), util.ToBE(u2f_SW_NO_ERROR)...)
	case u2f_COMMAND_REGISTER:
		response = server.handleU2FRegister(header, request)
	case u2f_COMMAND_AUTHENTICATE:
		response = server.handleU2FAuthenticate(header, request)
	default:
		panic(fmt.Sprintf("Invalid U2F Command: %#v", header))
	}
	u2fLogger.Printf("U2F RESPONSE: %#v\n\n", response)
	return response
}

type KeyHandle struct {
	PrivateKey    []byte `cbor:"1,keyasint"`
	ApplicationID []byte `cbor:"2,keyasint"`
}

func (server *u2fServer) sealKeyHandle(keyHandle *KeyHandle) []byte {
	box := seal(server.client.SealingEncryptionKey(), util.MarshalCBOR(keyHandle))
	return util.MarshalCBOR(box)
}

func (server *u2fServer) openKeyHandle(boxBytes []byte) (*KeyHandle, error) {
	var box encryptedBox
	err := cbor.Unmarshal(boxBytes, &box)
	if err != nil {
		return nil, err
	}
	data := open(server.client.SealingEncryptionKey(), box)
	var keyHandle KeyHandle
	err = cbor.Unmarshal(data, &keyHandle)
	if err != nil {
		return nil, err
	}
	return &keyHandle, nil
}

func (server *u2fServer) handleU2FRegister(header u2fMessageHeader, request []byte) []byte {
	challenge := request[:32]
	application := request[32:]
	util.Assert(len(challenge) == 32, "Challenge is not 32 bytes")
	util.Assert(len(application) == 32, "Application is not 32 bytes")

	privateKey := server.client.NewPrivateKey()
	encodedPublicKey := elliptic.Marshal(elliptic.P256(), privateKey.PublicKey.X, privateKey.PublicKey.Y)
	encodedPrivateKey, err := x509.MarshalECPrivateKey(privateKey)
	util.CheckErr(err, "Could not encode private key")

	unencryptedKeyHandle := KeyHandle{PrivateKey: encodedPrivateKey, ApplicationID: application}
	keyHandle := server.sealKeyHandle(&unencryptedKeyHandle)
	u2fLogger.Printf("KEY HANDLE: %d %#v\n\n", len(keyHandle), keyHandle)

	if !server.client.ApproveU2FRegistration(&unencryptedKeyHandle) {
		return util.ToBE(u2f_SW_CONDITIONS_NOT_SATISFIED)
	}

	cert := server.client.CreateAttestationCertificiate(privateKey)

	signatureDataBytes := util.Flatten([][]byte{{0}, application, challenge, keyHandle, encodedPublicKey})
	signature := sign(privateKey, signatureDataBytes)

	return util.Flatten([][]byte{{0x05}, encodedPublicKey, {uint8(len(keyHandle))}, keyHandle, cert, signature, util.ToBE(u2f_SW_NO_ERROR)})
}

func (server *u2fServer) handleU2FAuthenticate(header u2fMessageHeader, request []byte) []byte {
	requestReader := bytes.NewBuffer(request)
	control := u2fAuthenticateControl(header.Param1)
	challenge := util.Read(requestReader, 32)
	application := util.Read(requestReader, 32)

	keyHandleLength := util.ReadLE[uint8](requestReader)
	encryptedKeyHandleBytes := util.Read(requestReader, uint(keyHandleLength))
	keyHandle, err := server.openKeyHandle(encryptedKeyHandleBytes)
	if err != nil {
		u2fLogger.Printf("U2F AUTHENTICATE: Invalid key handle given - %s %#v\n\n", err, encryptedKeyHandleBytes)
		return util.ToBE(u2f_SW_WRONG_DATA)
	}
	if keyHandle.PrivateKey == nil || bytes.Compare(keyHandle.ApplicationID, application) != 0 {
		u2fLogger.Printf("U2F AUTHENTICATE: Invalid input data %#v\n\n", keyHandle)
		return util.ToBE(u2f_SW_WRONG_DATA)
	}
	privateKey, err := x509.ParseECPrivateKey(keyHandle.PrivateKey)
	util.CheckErr(err, "Could not decode private key")

	if control == u2f_AUTH_CONTROL_CHECK_ONLY {
		return util.ToBE(u2f_SW_CONDITIONS_NOT_SATISFIED)
	} else if control == u2f_AUTH_CONTROL_ENFORCE_USER_PRESENCE_AND_SIGN || control == u2f_AUTH_CONTROL_SIGN {
		if control == u2f_AUTH_CONTROL_ENFORCE_USER_PRESENCE_AND_SIGN {
			if !server.client.ApproveU2FAuthentication(keyHandle) {
				return util.ToBE(u2f_SW_CONDITIONS_NOT_SATISFIED)
			}
		}
		counter := server.client.NewAuthenticationCounterId()
		signatureDataBytes := util.Flatten([][]byte{application, {1}, util.ToBE(counter), challenge})
		signature := sign(privateKey, signatureDataBytes)
		return util.Flatten([][]byte{{1}, util.ToBE(counter), signature, util.ToBE(u2f_SW_NO_ERROR)})
	} else {
		// No error specific to invalid control byte, so return WRONG_LENGTH to indicate data error
		return util.ToBE(u2f_SW_WRONG_LENGTH)
	}
}
