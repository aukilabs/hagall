# Hagall Entity Component System

Hagall Entity Component System (HECS) is a distributed version of an normal Entity Component System (ECS) in a game engine such as Unity.

## Glossary

**Entity**: In an ECS, the Entity is the concept of a general-purpose object that is uniquely identifiable. In many implementations this is simply an integer. Optionally an Entity can also hold a list of all of its components.

**Component**: A component represents some aspect we want to attach to Entities. For example, our Entities need to have a position and rotation. We could create a *Pose* Component that would hold the position and rotation. This could then be attached to an entity. (Another simple example is adding Hit Points to your game entity.)

**System**: A System is a first class citizen as it is the process that understands and acts upon all Entities with a certain Component. Continuing with the Pose example, there would now be a Pose System that only responsibility is handling entity poses updates.
Systems can add, remove or modify Components during runtime.

## Using the Hagall Entity Component System

Following protobuf messages are used to interact with the Hagall Entity Component System

- [EntityComponentTypeAddRequest](https://github.com/aukilabs/hagall-common/blob/d51b9126b4f16210ece18bf062f67ca1a635b3ae/messages/hagallpb/hagall.proto#L501): Adds a new Component type to HECS. This is the first step to register a new Component type.
- [EntityComponentTypeGetNameRequest](https://github.com/aukilabs/hagall-common/blob/d51b9126b4f16210ece18bf062f67ca1a635b3ae/messages/hagallpb/hagall.proto#L534): Used to query the tag/name of a Component type when the id is known. This does not create a new Component type if tag/name is unknown.
- [EntityComponentTypeGetIdRequest](https://github.com/aukilabs/hagall-common/blob/d51b9126b4f16210ece18bf062f67ca1a635b3ae/messages/hagallpb/hagall.proto#L567): Used to query the id of a Component type when the tag/name is know.
- [EntityComponentAddRequest](https://github.com/aukilabs/hagall-common/blob/d51b9126b4f16210ece18bf062f67ca1a635b3ae/messages/hagallpb/hagall.proto#L600): Attaches a new Component to entity.
- [EntityComponentUpdateRequest](https://github.com/aukilabs/hagall-common/blob/d51b9126b4f16210ece18bf062f67ca1a635b3ae/messages/hagallpb/hagall.proto#L700): Updates the Component of a certain entity with a new state.
