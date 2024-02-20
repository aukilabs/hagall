# Hagall

## Introduction

_Hagall means Hail in Old Norse._

Hagall is a Real-Time Networking Server responsible for processing, responding to and broadcasting networking messages to connected clients (participants) in a session similar to how a multiplayer networking engine handles message passing in a first-person-shooter game.

Hagall is, through its module system and Entity Component System, extensible, but at its core, a simple networking engine that manages 3 types of abstractions:

- **Session** - A session facilitates the communication and in-memory persistence of participants, entities and actions inside an OpenGL coordinate system in unit meters. A session is similar to an FPS game session. Participants' positional data and actions are sent and broadcast as quickly as possible, and only the messages that are required to retrieve the current state of the session are stored in memory to support late joiners. Multiple sessions can exist in the same Hagall server, and each session is identified by a string ID in the format `<hagall_id>x<session_id>` e.g. `5fx3a`.
- **Participant** - Represents a connected client e.g., a mobile device or other hardware that wishes to interact with entities and other participants in a session.
- **Entity** - An entity is an object in a session with a _Pose_ and an ID, and it is owned by a specific participant. Hagall does not care about what an entity represents. It could be a 3D asset, an audio source, or a particle system. It's up to the application implementer to map their game objects to corresponding entities.

The core responsibilities of Hagall are:

- Creation and deletion of sessions; participant authentication and participant joining/leaving sessions.
- Addition and deletion of entities.
- Broadcasting of messages to participants.

Every Hagall server needs a unique wallet to participate in the [posemesh economy](https://www.posemesh.org/#economy).

## Documentations

- [Video Tutorial](doc/videotutorial.md)
- [Server Operator's Manual](doc/operatormanual.md)
- [Minimum Requirements](doc/minimumrequirements.md)
- [Deployment](doc/deployment.md)
- [Testing](doc/testing.md)
- [Troubleshooting](doc/troubleshooting.md)
- [Admin Endpoints](doc/adminendpoints.md)
- [Hagall Entity Component System](doc/entitycomponentsystem.md)
