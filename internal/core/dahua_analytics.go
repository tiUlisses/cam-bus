// internal/core/dahua_analytics.go
package core

import "strings"

// Lista de códigos de evento Dahua (documentação que você colou)
var DahuaEventTypes = []string{
    "VideoMotion",
    "SmartMotionHuman",
    "SmartMotionVehicle",
    "VideoLoss",
    "VideoBlind",
    "AlarmLocal",
    "CrossLineDetection",
    "CrossRegionDetection",
    "LeftDetection",
    "TakenAwayDetection",
    "VideoAbnormalDetection",
    "FaceDetection",
    "AudioMutation",
    "AudioAnomaly",
    "VideoUnFocus",
    "WanderDetection",
    "RioterDetection",
    "ParkingDetection",
    "MoveDetection",
    "StorageNotExist",
    "StorageFailure",
    "StorageLowSpace",
    "AlarmOutput",
    "MDResult",
    "HeatImagingTemper",
    "CrowdDetection",
    "FireWarning",
    "FireWarningInfo",
}

var DahuaEventTypeSet = func() map[string]struct{} {
    m := make(map[string]struct{}, len(DahuaEventTypes))
    for _, t := range DahuaEventTypes {
        m[strings.ToLower(t)] = struct{}{}
    }
    return m
}()
