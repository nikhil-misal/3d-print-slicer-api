from fastapi import FastAPI, UploadFile, File, HTTPException, Query
from fastapi.middleware.cors import CORSMiddleware
from stl import mesh
import numpy as np
import tempfile
import os
import math

app = FastAPI(
    title="SabkiDesigns 3D Print Analysis API",
    version="2.0.0"
)

app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=False,
    allow_methods=["*"],
    allow_headers=["*"],
)

# ============================================================
# STANDARD MATERIAL DENSITIES
# Unit: grams per cubic centimeter (g/cm³)
# ============================================================

MATERIAL_DENSITIES = {
    "PLA": 1.24,
    "PLA+": 1.25,
    "PETG": 1.27,
    "ABS": 1.04,
    "TPU": 1.21,
    "NYLON": 1.15,
}

# ============================================================
# INTERNAL UNIT STANDARD
# ALL PROCESSING AFTER DETECTION IS DONE IN MILLIMETERS
# ============================================================

UNIT_CANDIDATES = {
    "mm": 1.0,
    "cm": 10.0,
    "inch": 25.4,
    "meter": 1000.0,
}

# Reasonable printable size range for automatic detection.
# These values are used only to score possible STL scales.
MIN_REASONABLE_SIZE_MM = 0.5
MAX_REASONABLE_SIZE_MM = 1000.0


@app.get("/")
def home():
    return {
        "success": True,
        "status": "online",
        "message": "3D Print Analysis API is running",
        "internal_standard_unit": "mm"
    }


# ============================================================
# HELPER: VALIDATE MODEL DIMENSIONS
# ============================================================

def get_model_dimensions(vectors):
    """
    Reads the raw STL coordinates and returns
    X, Y and Z dimensions.
    """

    min_values = vectors.min(axis=(0, 1))
    max_values = vectors.max(axis=(0, 1))

    dimensions = max_values - min_values

    x = float(dimensions[0])
    y = float(dimensions[1])
    z = float(dimensions[2])

    return x, y, z


# ============================================================
# AUTOMATIC UNIT / SCALE DETECTION
# ============================================================

def score_scale(dimensions_mm):
    """
    Gives a score to a possible normalized model size.

    Lower score = more reasonable scale.

    This does NOT magically read STL units.
    It compares possible standard scales and chooses
    the most reasonable printable interpretation.
    """

    dims = [abs(float(d)) for d in dimensions_mm]

    non_zero_dims = [
        d for d in dims
        if d > 0.000001
    ]

    if not non_zero_dims:
        return float("inf")

    max_dim = max(non_zero_dims)
    min_dim = min(non_zero_dims)

    score = 0.0

    # Reject extremely small models
    if max_dim < MIN_REASONABLE_SIZE_MM:
        score += 100000.0 + (
            MIN_REASONABLE_SIZE_MM - max_dim
        ) * 1000

    # Reject extremely large models
    if max_dim > MAX_REASONABLE_SIZE_MM:
        score += 100000.0 + (
            max_dim - MAX_REASONABLE_SIZE_MM
        )

    # Prefer common 3D printable sizes
    # Around 10 mm to 300 mm gets a better score
    if 10 <= max_dim <= 300:
        score -= 100

    # Small but still printable
    elif 1 <= max_dim < 10:
        score += 20

    # Larger printable model
    elif 300 < max_dim <= 500:
        score += 30

    # Penalize absurdly tiny geometry
    if min_dim < 0.05:
        score += 200

    # Mild preference for common desktop 3D print sizes
    target_size = 100.0

    if max_dim > 0:
        score += abs(
            math.log10(max_dim / target_size)
        ) * 10

    return score


def detect_best_unit(raw_dimensions):
    """
    Tests common STL scale interpretations.

    Returns the best scale and normalized dimensions in mm.
    """

    candidates = []

    for unit_name, scale_factor in UNIT_CANDIDATES.items():

        normalized_dimensions = [
            float(d) * scale_factor
            for d in raw_dimensions
        ]

        score = score_scale(
            normalized_dimensions
        )

        candidates.append({
            "detected_unit": unit_name,
            "scale_factor_to_mm": scale_factor,
            "dimensions_mm": normalized_dimensions,
            "score": score
        })

    candidates.sort(
        key=lambda item: item["score"]
    )

    return candidates[0], candidates


# ============================================================
# WEIGHT ESTIMATION
# ============================================================

def calculate_print_weight(
    solid_volume_cm3,
    density,
    infill_percent
):
    """
    Estimates actual FDM printed weight.

    This is an estimate.
    Exact slicer weight requires actual slicing settings:
    wall count, layer height, top/bottom layers,
    infill pattern, supports, etc.
    """

    solid_weight = (
        solid_volume_cm3 * density
    )

    # Approximate shell contribution
    shell_factor = 0.28

    # Infill material contribution
    infill_factor = (
        infill_percent / 100.0
    ) * 0.72

    material_usage_factor = (
        shell_factor + infill_factor
    )

    # Never exceed 100% solid material
    material_usage_factor = min(
        material_usage_factor,
        1.0
    )

    # Minimum protection
    material_usage_factor = max(
        material_usage_factor,
        0.01
    )

    estimated_weight = (
        solid_weight *
        material_usage_factor
    )

    return (
        solid_weight,
        estimated_weight,
        material_usage_factor
    )


# ============================================================
# MAIN ANALYSIS API
# ============================================================

@app.post("/analyze")
async def analyze_stl(
    file: UploadFile = File(...),
    material: str = Query("PLA"),
    infill: float = Query(20)
):

    # --------------------------------------------------------
    # FILE VALIDATION
    # --------------------------------------------------------

    if not file.filename:
        raise HTTPException(
            status_code=400,
            detail="No file name received"
        )

    if not file.filename.lower().endswith(".stl"):
        raise HTTPException(
            status_code=400,
            detail="Only STL files are supported"
        )

    # --------------------------------------------------------
    # MATERIAL VALIDATION
    # --------------------------------------------------------

    material_key = (
        material.strip().upper()
    )

    if material_key not in MATERIAL_DENSITIES:
        raise HTTPException(
            status_code=400,
            detail=(
                "Unsupported material. "
                f"Use one of: "
                f"{list(MATERIAL_DENSITIES.keys())}"
            )
        )

    # --------------------------------------------------------
    # INFILL VALIDATION
    # --------------------------------------------------------

    if infill < 0 or infill > 100:
        raise HTTPException(
            status_code=400,
            detail=(
                "Infill must be between 0 and 100"
            )
        )

    temp_path = None

    try:

        # ----------------------------------------------------
        # SAVE UPLOADED FILE TEMPORARILY
        # ----------------------------------------------------

        content = await file.read()

        if not content:
            raise HTTPException(
                status_code=400,
                detail="Uploaded STL file is empty"
            )

        with tempfile.NamedTemporaryFile(
            delete=False,
            suffix=".stl"
        ) as temp_file:

            temp_path = temp_file.name
            temp_file.write(content)

        # ----------------------------------------------------
        # LOAD STL
        # numpy-stl supports common binary and ASCII STL files
        # ----------------------------------------------------

        model = mesh.Mesh.from_file(
            temp_path
        )

        if (
            model is None or
            model.vectors is None or
            len(model.vectors) == 0
        ):
            raise ValueError(
                "STL contains no valid triangles"
            )

        # ----------------------------------------------------
        # RAW DIMENSIONS
        # ----------------------------------------------------

        raw_x, raw_y, raw_z = (
            get_model_dimensions(
                model.vectors
            )
        )

        raw_dimensions = [
            raw_x,
            raw_y,
            raw_z
        ]

        if max(raw_dimensions) <= 0:
            raise ValueError(
                "Model dimensions are zero"
            )

        # ----------------------------------------------------
        # AUTOMATIC UNIT DETECTION
        # ----------------------------------------------------

        best_scale, all_candidates = (
            detect_best_unit(
                raw_dimensions
            )
        )

        scale_factor = (
            best_scale[
                "scale_factor_to_mm"
            ]
        )

        normalized_x = (
            raw_x * scale_factor
        )

        normalized_y = (
            raw_y * scale_factor
        )

        normalized_z = (
            raw_z * scale_factor
        )

        # ----------------------------------------------------
        # VOLUME
        #
        # numpy-stl returns volume in cubic coordinate units.
        #
        # If coordinates are normalized by:
        # scale_factor
        #
        # volume must be multiplied by:
        # scale_factor³
        # ----------------------------------------------------

        raw_volume, _, _ = (
            model.get_mass_properties()
        )

        raw_volume = abs(
            float(raw_volume)
        )

        if (
            not math.isfinite(
                raw_volume
            ) or
            raw_volume <= 0
        ):
            raise ValueError(
                "Could not calculate a valid closed-model volume. "
                "The STL may be open, broken or non-manifold."
            )

        volume_mm3 = (
            raw_volume *
            (scale_factor ** 3)
        )

        volume_cm3 = (
            volume_mm3 / 1000.0
        )

        # ----------------------------------------------------
        # MATERIAL WEIGHT
        # ----------------------------------------------------

        density = (
            MATERIAL_DENSITIES[
                material_key
            ]
        )

        (
            solid_weight_g,
            estimated_weight_g,
            material_usage_factor
        ) = calculate_print_weight(
            volume_cm3,
            density,
            infill
        )

        # ----------------------------------------------------
        # RESPONSE
        # ----------------------------------------------------

        return {
            "success": True,

            "status": "model_analyzed",

            "file_name":
                file.filename,

            "processing_unit":
                "mm",

            "material":
                material_key,

            "infill_percent":
                round(infill, 2),

            "automatic_scale_detection": {
                "enabled": True,

                "detected_interpretation":
                    best_scale[
                        "detected_unit"
                    ],

                "scale_factor_to_mm":
                    scale_factor,

                "customer_unit_input_required":
                    False
            },

            "dimensions": {
                "x_mm":
                    round(normalized_x, 2),

                "y_mm":
                    round(normalized_y, 2),

                "z_mm":
                    round(normalized_z, 2)
            },

            "volume": {
                "mm3":
                    round(volume_mm3, 2),

                "cm3":
                    round(volume_cm3, 4)
            },

            "density_g_cm3":
                density,

            "solid_weight_g":
                round(solid_weight_g, 2),

            "estimated_print_weight_g":
                round(estimated_weight_g, 2),

            "calculation": {
                "material_usage_factor":
                    round(
                        material_usage_factor,
                        4
                    ),

                "internal_standard":
                    "All models are normalized to millimeters before processing"
            },

            "message":
                "STL processed successfully with automatic scale normalization"
        }

    except HTTPException:
        raise

    except Exception as e:

        raise HTTPException(
            status_code=500,
            detail=(
                "Could not process STL file: "
                f"{str(e)}"
            )
        )

    finally:

        if (
            temp_path and
            os.path.exists(temp_path)
        ):
            os.remove(
                temp_path
            )
