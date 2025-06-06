package types

import (
	"errors"
	"fmt"
	"reflect"

	"github.com/cosmos/gogoproto/jsonpb"
	"github.com/cosmos/gogoproto/proto"
	gogoprotoany "github.com/cosmos/gogoproto/types/any"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"

	"cosmossdk.io/x/tx/signing"
)

var (

	// MaxUnpackAnySubCalls extension point that defines the maximum number of sub-calls allowed during the unpacking
	// process of protobuf Any messages.
	MaxUnpackAnySubCalls = 100

	// MaxUnpackAnyRecursionDepth extension point that defines the maximum allowed recursion depth during protobuf Any
	// message unpacking.
	MaxUnpackAnyRecursionDepth = 10
)

// AnyUnpacker is an alias for github.com/cosmos/gogoproto/types/any.AnyUnpacker.
type AnyUnpacker = gogoprotoany.AnyUnpacker

// UnpackInterfaces is a convenience function that calls UnpackInterfaces
// on x if x implements UnpackInterfacesMessage
func UnpackInterfaces(x interface{}, unpacker gogoprotoany.AnyUnpacker) error {
	if msg, ok := x.(gogoprotoany.UnpackInterfacesMessage); ok {
		return msg.UnpackInterfaces(unpacker)
	}
	return nil
}

var protoMessageType = reflect.TypeOf((*proto.Message)(nil)).Elem()

// InterfaceRegistry provides a mechanism for registering interfaces and
// implementations that can be safely unpacked from Any
type InterfaceRegistry interface {
	gogoprotoany.AnyUnpacker
	jsonpb.AnyResolver

	// RegisterInterface associates protoName as the public name for the
	// interface passed in as iface. This is to be used primarily to create
	// a public facing registry of interface implementations for clients.
	// protoName should be a well-chosen public facing name that remains stable.
	// RegisterInterface takes an optional list of impls to be registered
	// as implementations of iface.
	//
	// Ex:
	//   registry.RegisterInterface("cosmos.base.v1beta1.Msg", (*sdk.Msg)(nil))
	RegisterInterface(protoName string, iface any, impls ...proto.Message)

	// RegisterImplementations registers impls as concrete implementations of
	// the interface iface.
	//
	// Ex:
	//  registry.RegisterImplementations((*sdk.Msg)(nil), &MsgSend{}, &MsgMultiSend{})
	RegisterImplementations(iface any, impls ...proto.Message)

	// ListAllInterfaces list the type URLs of all registered interfaces.
	ListAllInterfaces() []string

	// ListImplementations lists the valid type URLs for the given interface name that can be used
	// for the provided interface type URL.
	ListImplementations(ifaceTypeURL string) []string

	// EnsureRegistered ensures there is a registered interface for the given concrete type.
	EnsureRegistered(iface any) error

	protodesc.Resolver

	// RangeFiles iterates over all registered files and calls f on each one. This
	// implements the part of protoregistry.Files that is needed for reflecting over
	// the entire FileDescriptorSet.
	RangeFiles(f func(protoreflect.FileDescriptor) bool)

	SigningContext() *signing.Context

	// mustEmbedInterfaceRegistry requires that all implementations of InterfaceRegistry embed an official implementation
	// from this package. This allows new methods to be added to the InterfaceRegistry interface without breaking
	// backwards compatibility.
	mustEmbedInterfaceRegistry()
}

// UnpackInterfacesMessage is meant to extend protobuf types (which implement
// proto.Message) to support a post-deserialization phase which unpacks
// types packed within Any's using the whitelist provided by AnyUnpacker
type UnpackInterfacesMessage interface {
	// UnpackInterfaces is implemented in order to unpack values packed within
	// Any's using the AnyUnpacker. It should generally be implemented as
	// follows:
	//   func (s *MyStruct) UnpackInterfaces(unpacker AnyUnpacker) error {
	//		var x AnyInterface
	//		// where X is an Any field on MyStruct
	//		err := unpacker.UnpackAny(s.X, &x)
	//		if err != nil {
	//			return nil
	//		}
	//		// where Y is a field on MyStruct that implements UnpackInterfacesMessage itself
	//		err = s.Y.UnpackInterfaces(unpacker)
	//		if err != nil {
	//			return nil
	//		}
	//		return nil
	//	 }
	UnpackInterfaces(unpacker gogoprotoany.AnyUnpacker) error
}

type interfaceRegistry struct {
	signing.ProtoFileResolver
	interfaceNames map[string]reflect.Type
	interfaceImpls map[reflect.Type]interfaceMap
	implInterfaces map[reflect.Type]reflect.Type
	typeURLMap     map[string]reflect.Type
	signingCtx     *signing.Context
}

type interfaceMap = map[string]reflect.Type

// NewInterfaceRegistry returns a new InterfaceRegistry
func NewInterfaceRegistry() InterfaceRegistry {
	registry, err := NewInterfaceRegistryWithOptions(InterfaceRegistryOptions{
		ProtoFiles: proto.HybridResolver,
		SigningOptions: signing.Options{
			AddressCodec:          failingAddressCodec{},
			ValidatorAddressCodec: failingAddressCodec{},
		},
	})
	if err != nil {
		panic(err)
	}
	return registry
}

// InterfaceRegistryOptions are options for creating a new InterfaceRegistry.
type InterfaceRegistryOptions struct {
	// ProtoFiles is the set of files to use for the registry. It is required.
	ProtoFiles signing.ProtoFileResolver

	// SigningOptions are the signing options to use for the registry.
	SigningOptions signing.Options
}

// NewInterfaceRegistryWithOptions returns a new InterfaceRegistry with the given options.
func NewInterfaceRegistryWithOptions(options InterfaceRegistryOptions) (InterfaceRegistry, error) {
	if options.ProtoFiles == nil {
		return nil, fmt.Errorf("proto files must be provided")
	}

	options.SigningOptions.FileResolver = options.ProtoFiles
	signingCtx, err := signing.NewContext(options.SigningOptions)
	if err != nil {
		return nil, err
	}

	return &interfaceRegistry{
		interfaceNames:    map[string]reflect.Type{},
		interfaceImpls:    map[reflect.Type]interfaceMap{},
		implInterfaces:    map[reflect.Type]reflect.Type{},
		typeURLMap:        map[string]reflect.Type{},
		ProtoFileResolver: options.ProtoFiles,
		signingCtx:        signingCtx,
	}, nil
}

func (registry *interfaceRegistry) RegisterInterface(protoName string, iface any, impls ...proto.Message) {
	typ := reflect.TypeOf(iface)
	if typ.Elem().Kind() != reflect.Interface {
		panic(fmt.Errorf("%T is not an interface type", iface))
	}

	registry.interfaceNames[protoName] = typ
	registry.RegisterImplementations(iface, impls...)
}

// EnsureRegistered ensures there is a registered interface for the given concrete type.
//
// Returns an error if not, and nil if so.
func (registry *interfaceRegistry) EnsureRegistered(impl any) error {
	if reflect.ValueOf(impl).Kind() != reflect.Ptr {
		return fmt.Errorf("%T is not a pointer", impl)
	}

	if _, found := registry.implInterfaces[reflect.TypeOf(impl)]; !found {
		return fmt.Errorf("%T does not have a registered interface", impl)
	}

	return nil
}

// RegisterImplementations registers a concrete proto Message which implements
// the given interface.
//
// This function PANICs if different concrete types are registered under the
// same typeURL.
func (registry *interfaceRegistry) RegisterImplementations(iface any, impls ...proto.Message) {
	for _, impl := range impls {
		typeURL := MsgTypeURL(impl)
		registry.registerImpl(iface, typeURL, impl)
	}
}

// RegisterCustomTypeURL registers a concrete type which implements the given
// interface under `typeURL`.
//
// This function PANICs if different concrete types are registered under the
// same typeURL.
func (registry *interfaceRegistry) RegisterCustomTypeURL(iface any, typeURL string, impl proto.Message) {
	registry.registerImpl(iface, typeURL, impl)
}

// registerImpl registers a concrete type which implements the given
// interface under `typeURL`.
//
// This function PANICs if different concrete types are registered under the
// same typeURL.
func (registry *interfaceRegistry) registerImpl(iface any, typeURL string, impl proto.Message) {
	ityp := reflect.TypeOf(iface).Elem()
	imap, found := registry.interfaceImpls[ityp]
	if !found {
		imap = map[string]reflect.Type{}
	}

	implType := reflect.TypeOf(impl)
	if !implType.AssignableTo(ityp) {
		panic(fmt.Errorf("type %T doesn't actually implement interface %+v", impl, ityp))
	}

	// Check if we already registered something under the given typeURL. It's
	// okay to register the same concrete type again, but if we are registering
	// a new concrete type under the same typeURL, then we throw an error (here,
	// we panic).
	foundImplType, found := imap[typeURL]
	if found && foundImplType != implType {
		panic(
			fmt.Errorf(
				"concrete type %s has already been registered under typeURL %s, cannot register %s under same typeURL. "+
					"This usually means that there are conflicting modules registering different concrete types "+
					"for a same interface implementation",
				foundImplType,
				typeURL,
				implType,
			),
		)
	}

	imap[typeURL] = implType
	registry.typeURLMap[typeURL] = implType
	registry.implInterfaces[implType] = ityp
	registry.interfaceImpls[ityp] = imap
}

func (registry *interfaceRegistry) ListAllInterfaces() []string {
	interfaceNames := registry.interfaceNames
	keys := make([]string, 0, len(interfaceNames))
	for key := range interfaceNames {
		keys = append(keys, key)
	}
	return keys
}

func (registry *interfaceRegistry) ListImplementations(ifaceName string) []string {
	typ, ok := registry.interfaceNames[ifaceName]
	if !ok {
		return []string{}
	}

	impls, ok := registry.interfaceImpls[typ.Elem()]
	if !ok {
		return []string{}
	}

	keys := make([]string, 0, len(impls))
	for key := range impls {
		keys = append(keys, key)
	}
	return keys
}

func (registry *interfaceRegistry) UnpackAny(any *Any, iface any) error {
	unpacker := &statefulUnpacker{
		registry: registry,
		maxDepth: MaxUnpackAnyRecursionDepth,
		maxCalls: &sharedCounter{count: MaxUnpackAnySubCalls},
	}
	return unpacker.UnpackAny(any, iface)
}

// sharedCounter is a type that encapsulates a counter value
type sharedCounter struct {
	count int
}

// statefulUnpacker is a struct that helps in deserializing and unpacking
// protobuf Any messages while maintaining certain stateful constraints.
type statefulUnpacker struct {
	registry *interfaceRegistry
	maxDepth int
	maxCalls *sharedCounter
}

// cloneForRecursion returns a new statefulUnpacker instance with maxDepth reduced by one, preserving the registry and maxCalls.
func (r statefulUnpacker) cloneForRecursion() *statefulUnpacker {
	return &statefulUnpacker{
		registry: r.registry,
		maxDepth: r.maxDepth - 1,
		maxCalls: r.maxCalls,
	}
}

// UnpackAny deserializes a protobuf Any message into the provided interface, ensuring the interface is a pointer.
// It applies stateful constraints such as max depth and call limits, and unpacks interfaces if required.
func (r *statefulUnpacker) UnpackAny(any *Any, iface interface{}) error {
	if r.maxDepth <= 0 {
		return errors.New("max depth exceeded")
	}
	if r.maxCalls.count <= 0 {
		return errors.New("call limit exceeded")
	}
	// here we gracefully handle the case in which `any` itself is `nil`, which may occur in message decoding
	if any == nil {
		return nil
	}

	if any.TypeUrl == "" {
		// if TypeUrl is empty return nil because without it we can't actually unpack anything
		return nil
	}

	r.maxCalls.count--

	rv := reflect.ValueOf(iface)
	if rv.Kind() != reflect.Ptr {
		return errors.New("UnpackAny expects a pointer")
	}

	rt := rv.Elem().Type()

	cachedValue := any.GetCachedValue()
	if cachedValue != nil {
		if reflect.TypeOf(cachedValue).AssignableTo(rt) {
			rv.Elem().Set(reflect.ValueOf(cachedValue))
			return nil
		}
	}

	imap, found := r.registry.interfaceImpls[rt]
	if !found {
		return fmt.Errorf("no registered implementations of type %+v", rt)
	}

	typ, found := imap[any.TypeUrl]
	if !found {
		return fmt.Errorf("no concrete type registered for type URL %s against interface %T", any.TypeUrl, iface)
	}

	// Firstly check if the type implements proto.Message to avoid
	// unnecessary invocations to reflect.New
	if !typ.Implements(protoMessageType) {
		return fmt.Errorf("can't proto unmarshal %T", typ)
	}

	msg := reflect.New(typ.Elem()).Interface().(proto.Message)
	err := proto.Unmarshal(any.Value, msg)
	if err != nil {
		return err
	}

	err = UnpackInterfaces(msg, r.cloneForRecursion())
	if err != nil {
		return err
	}

	rv.Elem().Set(reflect.ValueOf(msg))

	newAnyWithCache, err := NewAnyWithValue(msg)
	if err != nil {
		return err
	}

	*any = *newAnyWithCache
	return nil
}

// Resolve returns the proto message given its typeURL. It works with types
// registered with RegisterInterface/RegisterImplementations, as well as those
// registered with RegisterWithCustomTypeURL.
func (registry *interfaceRegistry) Resolve(typeURL string) (proto.Message, error) {
	typ, found := registry.typeURLMap[typeURL]
	if !found {
		return nil, fmt.Errorf("unable to resolve type URL %s", typeURL)
	}

	msg, ok := reflect.New(typ.Elem()).Interface().(proto.Message)
	if !ok {
		return nil, fmt.Errorf("can't resolve type URL %s", typeURL)
	}

	return msg, nil
}

func (registry *interfaceRegistry) SigningContext() *signing.Context {
	return registry.signingCtx
}

func (registry *interfaceRegistry) mustEmbedInterfaceRegistry() {}

type failingAddressCodec struct{}

func (f failingAddressCodec) StringToBytes(string) ([]byte, error) {
	return nil, fmt.Errorf("InterfaceRegistry requires a proper address codec implementation to do address conversion")
}

func (f failingAddressCodec) BytesToString([]byte) (string, error) {
	return "", fmt.Errorf("InterfaceRegistry requires a proper address codec implementation to do address conversion")
}
