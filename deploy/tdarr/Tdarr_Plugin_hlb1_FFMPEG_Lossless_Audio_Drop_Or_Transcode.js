/* eslint-disable */
// Drop redundant lossless tracks, otherwise transcode them to the kept codec.
// Replaces Community:Tdarr_Plugin_a9hd_FFMPEG_Transcode_Specific_Audio_Stream_Codecs
// on the "Shrink" flow's audio step. Two branches:
//   - lossless tracks AND a usable (non-commentary) kept-codec track exist ->
//     REMUX dropping the lossless tracks (faster, recovers more space);
//   - lossless tracks without a usable kept-codec track -> transcode them,
//     exactly as a9hd did.
const details = () => {
  return {
    id: "Tdarr_Plugin_hlb1_FFMPEG_Lossless_Audio_Drop_Or_Transcode",
    Stage: "Pre-processing",
    Name: "Lossless audio: drop when kept codec exists, else transcode",
    Type: "Audio",
    Operation: "Transcode",
    Description:
      `[Contains built-in filter] For files with lossless audio (TrueHD/DTS-HD/DTS): ` +
      `if a non-commentary track in the kept codec (default eac3) already exists, ` +
      `the lossless tracks are DROPPED (pure remux); otherwise they are transcoded ` +
      `to the target codec at the given bitrate (the a9hd behaviour). Commentary ` +
      `tracks (title regex or disposition flag) never count as the kept track.\n\n`,
    Version: "1.00",
    Tags: "pre-processing,audio only,ffmpeg,configurable",
    Inputs: [
      {
        name: "lossless_codecs",
        type: 'string',
        defaultValue: 'truehd,dts',
        inputUI: { type: 'text' },
        tooltip: `Comma-separated lossless codecs to drop or transcode.
        \\nExample:\\n
        truehd,dts
        `,
      },
      {
        name: "kept_codec",
        type: 'string',
        defaultValue: 'eac3',
        inputUI: { type: 'text' },
        tooltip: `An existing non-commentary track in this codec makes the lossless ones redundant (they get dropped).
        \\nExample:\\n
        eac3
        `,
      },
      {
        name: "transcode_bitrate",
        type: 'string',
        defaultValue: '640k',
        inputUI: { type: 'text' },
        tooltip: `Bitrate for the transcode branch (no kept-codec track present).
        \\nExample:\\n
        640k
        `,
      },
      {
        name: "commentary_regex",
        type: 'string',
        defaultValue: 'commentary',
        inputUI: { type: 'text' },
        tooltip: `Case-insensitive regex matched against audio track titles; a match marks
        the track as commentary so it never counts as the kept track. Tracks with
        disposition.comment=1 always count as commentary.
        \\nExample:\\n
        commentary
        `,
      },
    ],
  };
};

// eslint-disable-next-line @typescript-eslint/no-unused-vars
const plugin = (file, librarySettings, inputs, otherArguments) => {
  const lib = require('../methods/lib')();
  // eslint-disable-next-line @typescript-eslint/no-unused-vars,no-param-reassign
  inputs = lib.loadDefaultValues(inputs, details);
  const response = {
    processFile: false,
    preset: "",
    container: ".mp4",
    handBrakeMode: false,
    FFmpegMode: false,
    reQueueAfter: false,
    infoLog: "",
  };

  const losslessCodecs = String(inputs.lossless_codecs || '')
    .split(',')
    .map((c) => c.trim().toLowerCase())
    .filter((c) => c.length > 0);
  const keptCodec = String(inputs.kept_codec || '').trim().toLowerCase();
  const bitrate = String(inputs.transcode_bitrate || '').trim();
  let commentaryRe;
  try {
    commentaryRe = new RegExp(String(inputs.commentary_regex || ''), 'i');
  } catch (e) {
    response.infoLog += "☒ commentary_regex is not a valid regex! \n";
    return response;
  }
  if (!losslessCodecs.length || !keptCodec) {
    response.infoLog += "☒ Inputs not entered! \n";
    return response;
  }

  // ffProbeData.streams positions ARE the ffmpeg INPUT indexes (0:N) — valid
  // for -map only, never for -c:N (see branch 2).
  const audio = [];
  (file.ffProbeData && file.ffProbeData.streams ? file.ffProbeData.streams : [])
    .forEach((s, i) => {
      if (String(s.codec_type || '').toLowerCase() !== 'audio') {
        return;
      }
      const title = String((s.tags && s.tags.title) || '');
      const isCommentary =
        commentaryRe.test(title) ||
        (s.disposition && s.disposition.comment === 1);
      audio.push({ index: i, codec: String(s.codec_name || '').toLowerCase(), title, isCommentary });
    });

  const lossless = audio.filter((a) => losslessCodecs.includes(a.codec));
  if (!lossless.length) {
    response.infoLog += "☑ File does not have any lossless streams to consider! \n";
    return response;
  }

  const usableKept = audio.filter((a) => a.codec === keptCodec && !a.isCommentary);

  if (usableKept.length) {
    // Branch 1: pure remux, drop every lossless track. Negative mapping per
    // jordy/sdd3/Migz3 community plugins. usableKept non-empty guarantees at
    // least one audio stream survives.
    let ffmpegCommand = `, -map 0 `;
    lossless.forEach((a) => {
      ffmpegCommand += `-map -0:${a.index} `;
    });
    ffmpegCommand += `-c copy -max_muxing_queue_size 9999`;
    response.processFile = true;
    response.preset = ffmpegCommand;
    response.container = "." + file.container;
    response.handBrakeMode = false;
    response.FFmpegMode = true;
    response.reQueueAfter = true;
    response.infoLog +=
      `☒ Dropping ${lossless.map((a) => a.codec).join(",")} track(s): ` +
      `non-commentary ${keptCodec} already present (${usableKept.length}). \n`;
    return response;
  }

  // Branch 2: no usable kept-codec track — transcode the lossless streams
  // (the a9hd behaviour, restricted to the matched codecs).
  //
  // Mapping rules for embedded cover art:
  //   - `-map 0:v` matches EVERY video stream, including embedded cover-art
  //     (attached picture) streams common in REMUXes. Each extra video stream
  //     shifts every following output position by one, and `-c:N` then lands
  //     the audio encoder on the cover image (`Invalid encoder type 'eac3'`).
  //     `-map 0:V:0` takes exactly one real video stream — V excludes
  //     attached pictures — wherever the cover sits in the input.
  //   - `-c:N` is an OUTPUT specifier: N counts mapped outputs, not input
  //     indexes. A running counter assigns each audio stream its output slot,
  //     starting after the video stream mapped above.
  const hasVideo = (file.ffProbeData && file.ffProbeData.streams
    ? file.ffProbeData.streams : [])
    .some((s) => String(s.codec_type || '').toLowerCase() === 'video');
  let out = hasVideo ? 1 : 0;
  let ffmpegCommand = `, -c copy ${hasVideo ? '-map 0:V:0 ' : ''}`;
  audio.forEach((a) => {
    if (lossless.includes(a)) {
      ffmpegCommand += `-map 0:${a.index} -c:${out} ${keptCodec} `;
      if (bitrate !== '') {
        ffmpegCommand += `-b:a ${bitrate} `;
      }
    } else {
      ffmpegCommand += `-map 0:${a.index} `;
    }
    out++;
  });
  ffmpegCommand += `-map 0:s? -map 0:d? -max_muxing_queue_size 9999`;
  response.processFile = true;
  response.preset = ffmpegCommand;
  response.container = "." + file.container;
  response.handBrakeMode = false;
  response.FFmpegMode = true;
  response.reQueueAfter = true;
  response.infoLog +=
    `☒ Lossless tracks present but no non-commentary ${keptCodec} — ` +
    `transcoding ${lossless.map((a) => a.codec).join(",")} to ${keptCodec}. \n`;
  return response;
};

module.exports.details = details;
module.exports.plugin = plugin;
