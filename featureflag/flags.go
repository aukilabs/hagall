package featureflag

type Flag string

const (
	FlagDisableSessionState                   Flag = "DISABLE_SESSION_STATE"
	FlagDisableParticipantJoinBroadcast       Flag = "DISABLE_PARTICIPANT_JOIN_BROADCAST"
	FlagDisableParticipantLeaveBroadcast      Flag = "DISABLE_PARTICIPANT_LEAVE_BROADCAST"
	FlagDisableEntityAddBroadcast             Flag = "DISABLE_ENTITY_ADD_BROADCAST"
	FlagDisableEntityDeleteBroadcast          Flag = "DISABLE_ENTITY_DELETE_BROADCAST"
	FlagDisableEntityUpdatePoseBroadcast      Flag = "DISABLE_ENTITY_UPDATE_POSE_BROADCAST"
	FlagDisableCustomMessageBroadcast         Flag = "DISABLE_CUSTOM_MESSAGE_BROADCAST"
	FlagDisableEntityComponentAddBroadcast    Flag = "DISABLE_ENTITY_COMPONENT_ADD_BROADCAST"
	FlagDisableEntityComponentUpdateBroadcast Flag = "DISABLE_ENTITY_COMPONENT_UPDATE_BROADCAST"
	FlagDisableEntityComponentDeleteBroadcast Flag = "DISABLE_ENTITY_COMPONENT_DELETE_BROADCAST"
)
