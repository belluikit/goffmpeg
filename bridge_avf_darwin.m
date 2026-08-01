//go:build darwin && !ios

// AVFoundation capture-mode enumeration.
//
// This exists because libavdevice will not tell a program what a camera
// accepts. avfoundation works it out, prints it — "Supported modes:" and a
// list — and then fails the open with a code that carries none of it (#5). It
// also discards its own AVERROR(EINVAL) on the way out, so a rejected
// framerate is indistinguishable from a missing device. The data is in
// AVCaptureDevice, so it is read here directly rather than recovered from a
// log line.
//
// Its own translation unit because it is Objective-C. The _darwin suffix is
// not sufficient on its own: GOOS=ios also matches it, and the mobile targets
// are built --disable-avdevice with no device support at all, hence the
// explicit !ios above.

#import <AVFoundation/AVFoundation.h>
#import <CoreMedia/CoreMedia.h>

#include "bridge.h"

// avf_pixel_format_name maps a CoreVideo pixel format to the name libav uses.
//
// This mirrors avf_pixel_formats in FFmpeg's avfoundation.m entry for entry,
// and that is the point rather than an accident of convenience. pixel_format=
// does not name the camera's native layout; it names what avfoundation will
// *deliver*, and it is validated against that table. So the only names worth
// emitting are the ones in it — anything else is a string OpenDevice will
// reject, which would make DeviceMode.Options() produce a mode that cannot be
// opened, defeating the purpose.
//
// The traps are the 32-bit RGB entries: 32BGRA is bgr0 and not bgra, 32ARGB is
// 0rgb and not argb, because the alpha channel is discarded rather than
// carried. A first version of this guessed the intuitive names and every
// round trip through Options() failed.
//
// A format absent from the table yields NULL. 420YpCbCr8BiPlanarFullRange is
// the case that matters — FFmpeg maps only the video-range variant — and a
// mode is still worth reporting for its resolution and framerate without a
// pixel format it would refuse.
static const char *avf_pixel_format_name(OSType t) {
    switch (t) {
        case kCVPixelFormatType_1Monochrome:                  return "monob";
        case kCVPixelFormatType_16BE555:                      return "rgb555be";
        case kCVPixelFormatType_16LE555:                      return "rgb555le";
        case kCVPixelFormatType_16BE565:                      return "rgb565be";
        case kCVPixelFormatType_16LE565:                      return "rgb565le";
        case kCVPixelFormatType_24RGB:                        return "rgb24";
        case kCVPixelFormatType_24BGR:                        return "bgr24";
        case kCVPixelFormatType_32ARGB:                       return "0rgb";
        case kCVPixelFormatType_32BGRA:                       return "bgr0";
        case kCVPixelFormatType_32ABGR:                       return "0bgr";
        case kCVPixelFormatType_32RGBA:                       return "rgb0";
        case kCVPixelFormatType_48RGB:                        return "bgr48be";
        case kCVPixelFormatType_422YpCbCr8:                   return "uyvy422";
        case kCVPixelFormatType_4444YpCbCrA8R:                return "yuva444p";
        case kCVPixelFormatType_4444AYpCbCr16:                return "yuva444p16le";
        case kCVPixelFormatType_444YpCbCr8:                   return "yuv444p";
        case kCVPixelFormatType_422YpCbCr16:                  return "yuv422p16le";
        case kCVPixelFormatType_422YpCbCr10:                  return "yuv422p10le";
        case kCVPixelFormatType_444YpCbCr10:                  return "yuv444p10le";
        case kCVPixelFormatType_420YpCbCr8Planar:             return "yuv420p";
        case kCVPixelFormatType_420YpCbCr8BiPlanarVideoRange: return "nv12";
        case kCVPixelFormatType_422YpCbCr8_yuvs:              return "yuyv422";
        case kCVPixelFormatType_OneComponent8:                return "gray";
        default:                                              return NULL;
    }
}

// avf_device_for_url resolves the same selector decoder_open_device takes.
//
// avfoundation addresses cameras positionally — "0" is the first video device
// — because it implements no enumeration and there is no name to use. A
// non-numeric url is matched against the device's unique ID and then its
// localized name, which is what avfoundation.m itself accepts.
static AVCaptureDevice *avf_device_for_url(const char *url) {
    if (!url || !url[0]) return nil;

    NSString *sel = [NSString stringWithUTF8String:url];
    if (!sel) return nil;

    // "0", "1" … index into the video devices in the order avfoundation lists
    // them. AVCaptureDeviceTypeExternal replaced the deprecated
    // ...TypeExternalUnknown; both cover a USB camera.
    NSArray<AVCaptureDeviceType> *types = @[
        AVCaptureDeviceTypeBuiltInWideAngleCamera,
        AVCaptureDeviceTypeExternal,
    ];
    AVCaptureDeviceDiscoverySession *session =
        [AVCaptureDeviceDiscoverySession discoverySessionWithDeviceTypes:types
                                                               mediaType:AVMediaTypeVideo
                                                                position:AVCaptureDevicePositionUnspecified];
    NSArray<AVCaptureDevice *> *devices = [session devices];

    NSScanner *scanner = [NSScanner scannerWithString:sel];
    NSInteger index = 0;
    if ([scanner scanInteger:&index] && [scanner isAtEnd]) {
        if (index < 0 || index >= (NSInteger)[devices count]) return nil;
        return devices[index];
    }

    for (AVCaptureDevice *d in devices) {
        if ([[d uniqueID] isEqualToString:sel]) return d;
    }
    for (AVCaptureDevice *d in devices) {
        if ([[d localizedName] isEqualToString:sel]) return d;
    }
    return nil;
}

int goffmpeg_avf_collect_modes(const char *url, void *modes) {
    @autoreleasepool {
        AVCaptureDevice *device = avf_device_for_url(url);
        if (!device) return GOFFMPEG_DEVICE_MODES_NO_DEVICE;

        for (AVCaptureDeviceFormat *format in [device formats]) {
            CMFormatDescriptionRef desc = [format formatDescription];
            if (!desc) continue;

            CMVideoDimensions dim = CMVideoFormatDescriptionGetDimensions(desc);
            const char *pixfmt =
                avf_pixel_format_name(CMFormatDescriptionGetMediaSubType(desc));

            // A format with no advertised range still gets an entry: its
            // resolution is usable, and reporting 0fps is more honest than
            // dropping a mode the device does offer. device_modes_min_fps
            // documents 0 as "not reported".
            NSArray *ranges = [format videoSupportedFrameRateRanges];
            if ([ranges count] == 0) {
                // AVFoundation delivers raw frames, so this is never a codec name.
                device_modes_append(modes, dim.width, dim.height, 0, 0, pixfmt, 0);
                continue;
            }
            for (AVFrameRateRange *range in ranges) {
                device_modes_append(modes, dim.width, dim.height,
                                    [range minFrameRate], [range maxFrameRate],
                                    pixfmt, 0);
            }
        }
        return 0;
    }
}
